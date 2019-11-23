package main

import (
	"bytes"
	"crypto/tls"
	"errors"
	"flag"
	"io"
	"io/ioutil"
	"log"
	"net"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	jsoniter "github.com/json-iterator/go"

	"github.com/valyala/fasthttp"
)

var domainNameRegex = regexp.MustCompile(`^([a-zA-Z0-9_]{1}[a-zA-Z0-9_-]{0,62}){1}(\.[a-zA-Z0-9_]{1}[a-zA-Z0-9_-]{0,62})*[\._]?$`)
var json = jsoniter.ConfigCompatibleWithStandardLibrary

var dialerFunc = getDialer(&net.Dialer{
	Timeout:   7 * time.Second,
	KeepAlive: 5 * 60 * time.Second,
	DualStack: true,
})

func httpsHandler(ctx *fasthttp.RequestCtx, host, hostname string) error {
	var ioFrom net.Conn
	destConn, err := dialerFunc(host)
	if err != nil {
		return err
	}
	isMustProxify := mustProxify(hostname)
	log.Println("Proxify:", hostname, isMustProxify)
	if isMustProxify {
		destConnTLS := tls.Client(destConn, &tls.Config{
			InsecureSkipVerify:    true,
			ServerName:            config.sni,
			VerifyPeerCertificate: verifyPeerCertFunc(hostname),
		})
		err = destConnTLS.Handshake()
		if err == nil {
			ioFrom = destConnTLS
		} else {
			// return err
			log.Println("Dest handshake:", hostname, err)
			// fallback
			ioFrom, err = dialerFunc(host)
			if err != nil {
				return err
			}
			isMustProxify = false
		}
	} else {
		ioFrom = destConn
	}

	if ctx.Hijacked() {
		return errors.New(hostname + " hijacked")
	}
	ctx.SetStatusCode(fasthttp.StatusOK)
	ctx.Response.Header.Set("Connection", "keep-alive")
	ctx.Response.Header.Set("Keep-Alive", "timeout=10, max=10")
	ctx.Hijack(func(clientConn net.Conn) {
		var ioTo net.Conn
		if isMustProxify {
			tlsConfig, err := TLSConfigFromCA(&GoproxyCa, hostname)
			if err != nil {
				log.Println("TLSConfigFromCA:", hostname, err)
				return
			}
			clientConnTLS := tls.Server(clientConn, tlsConfig)
			err = clientConnTLS.Handshake()
			if err != nil {
				log.Println("TLSHandshake", hostname, err)
				return
			}
			ioTo = clientConnTLS
		} else {
			ioTo = clientConn
		}
		go ioTransfer(ioFrom, ioTo)
		ioTransfer(ioTo, ioFrom)
	})
	return nil
}

func ioTransfer(destination io.WriteCloser, source io.ReadCloser) {
	defer recover()
	_, err := io.Copy(destination, source)
	if err != nil {
		if err != io.EOF {
			// log.Println("ioTransfer", err)
		}
	}
}

func requestHandler(ctx *fasthttp.RequestCtx) {
	defer func() {
		if r := recover(); r != nil {
			log.Println(r, string(debug.Stack()))
		}
	}()
	// Some library must set header: Connection: keep-alive
	// ctx.Response.Header.Del("Connection")
	// ctx.Response.ConnectionClose() // ==> false

	// log.Println(string(ctx.Path()), string(ctx.Host()), ctx.String(), "\r\n\r\n", ctx.Request.String())

	host := string(ctx.Host())
	if host == "" {
		ctx.SetStatusCode(fasthttp.StatusBadRequest)
		log.Println("Reject: Empty host")
		return
	}

	hostname, _, err := net.SplitHostPort(host)
	if err != nil {
		if err1, ok := err.(*net.AddrError); ok && strings.Index(err1.Err, "missing port") != -1 {
			hostname, _, err = net.SplitHostPort(host + ":80")
		}
		if err != nil {
			ctx.SetStatusCode(fasthttp.StatusBadRequest)
			log.Println("Reject: Invalid host", host, err)
			return
		}
	}

	if ip := net.ParseIP(hostname); ip != nil {
		// if is ip
		// log.Println("This is IP")
		if isPrivateIP(ip) {
			ctx.SetStatusCode(fasthttp.StatusBadRequest)
			log.Println("Reject: Private IP", ip.String())
			return
		}
	} else if domainNameRegex.MatchString(hostname) {
		// if is domain
		// log.Println("This is domain")
		if strings.ToLower(hostname) == "localhost" {
			ctx.SetStatusCode(fasthttp.StatusBadRequest)
			log.Println("Reject: Localhost")
			return
		}
	} else {
		// invalid host
		ctx.SetStatusCode(fasthttp.StatusBadRequest)
		log.Println("Reject: Invalid host", host)
		return
	}

	// https connecttion
	if bytes.Equal(ctx.Method(), []byte("CONNECT")) {
		err = httpsHandler(ctx, host, hostname)
		if err != nil {
			ctx.SetStatusCode(fasthttp.StatusInternalServerError)
			log.Println("httpsHandler:", host, err)
		}
		return
	}

	err = httpClient.DoTimeout(&ctx.Request, &ctx.Response, 15*time.Second)
	if err != nil {
		log.Println("HTTPHandler:", host, err)
		ctx.SetStatusCode(fasthttp.StatusInternalServerError)
	}
}

var config struct {
	port              int
	sni               string
	skipTLSVerify     bool
	domainList        string
	domainRegexList   string
	domainCertMapFile string
}

var domainProxiesCache = map[string]bool{}
var domainProxiesCacheLock sync.RWMutex
var domainsRegex []*regexp.Regexp
var lineRegex = regexp.MustCompile(`[\r\n]+`)
var certDomainAliasMap = map[string][]string{}

func parseDomains() bool {
	if len(config.domainList) > 0 {
		c, err := ioutil.ReadFile(config.domainList)
		if err == nil {
			lines := lineRegex.Split(string(c), -1)
			for _, line := range lines {
				line = strings.Trim(line, "\r\n\t ")
				if len(line) < 1 {
					continue
				}
				domainProxiesCacheLock.Lock()
				domainProxiesCache[line] = true
				domainProxiesCacheLock.Unlock()
			}
		} else {
			log.Println(err)
		}
	}
	if len(config.domainRegexList) > 0 {
		c, err := ioutil.ReadFile(config.domainRegexList)
		if err == nil {
			lines := lineRegex.Split(string(c), -1)
			for _, line := range lines {
				line = strings.Trim(line, "\r\n\t ")
				if len(line) < 1 {
					continue
				}
				domainsRegex = append(domainsRegex, regexp.MustCompile(line))
			}
		} else {
			log.Println(err)
		}
	}
	if len(config.domainCertMapFile) > 0 {
		c, err := ioutil.ReadFile(config.domainCertMapFile)
		if err == nil {
			err = json.Unmarshal(c, &certDomainAliasMap)
		}
		if err != nil {
			log.Println(err)
		}
	}
	if len(domainsRegex) < 1 && len(domainProxiesCache) < 1 {
		log.Println("No domains to proxy? Please specify a domain name in", config.domainList, "or", config.domainRegexList)
		return false
	}
	return true
}

// OK, no lock need here
func mustProxify(hostname string) bool {
	domainProxiesCacheLock.RLock()
	b, ok := domainProxiesCache[hostname]
	domainProxiesCacheLock.RUnlock()
	if ok {
		return b
	}
	b = false
	for _, re := range domainsRegex {
		b = re.MatchString(hostname)
		if b {
			break
		}
	}
	domainProxiesCacheLock.Lock()
	domainProxiesCache[hostname] = b
	domainProxiesCacheLock.Unlock()
	return b
}

func main() {
	flag.IntVar(&config.port, "p", 8080, "listen port")
	flag.BoolVar(&config.skipTLSVerify, "k", false, "Skip TLS Cert Verification")
	flag.StringVar(&config.sni, "sni", "vinhja.xt", "Fake HTTPS SNI")
	flag.StringVar(&config.domainList, "d", "domains.txt", "Domains List File")
	flag.StringVar(&config.domainRegexList, "r", "domains-regex.txt", "Domains Regex List File")
	flag.StringVar(&config.domainCertMapFile, "dcm", "domains-certs.json", "Domains Cert Map File")
	log.Println("Config", config)
	flag.Parse()

	if parseDomains() == false {
		return
	}

	Server := &fasthttp.Server{
		Handler:              requestHandler,
		Name:                 "go-sniproxy",
		ReadTimeout:          10 * time.Second, // 120s
		WriteTimeout:         10 * time.Second,
		MaxKeepaliveDuration: 10 * time.Second,
		MaxRequestBodySize:   2 * 1024 * 1024, // 2MB
		DisableKeepalive:     false,
	}

	log.Println("Server running on:", config.port)
	if err := Server.ListenAndServe(":" + strconv.Itoa(config.port)); err != nil {
		log.Print("HTTP serve error:", err)
	}
}