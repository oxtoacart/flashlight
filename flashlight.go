// flashlight is a lightweight chained proxy that can run in client or server mode.
package main

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/getlantern/go-mitm/mitm"
	"github.com/oxtoacart/keyman"
)

const (
	CONNECT          = "CONNECT"
	X_LANTERN_HOST   = "X-Lantern-Host"
	X_LANTERN_SCHEME = "X-Lantern-Scheme"
)

var (
	help         = flag.Bool("help", false, "Get usage help")
	addr         = flag.String("addr", "", "ip:port on which to listen for requests.  When running as a client proxy, we'll listen with http, when running as a server proxy we'll listen with https")
	upstreamHost = flag.String("server", "", "hostname at which to connect to a server flashlight (always using https).  When specified, this flashlight will run as a client proxy, otherwise it runs as a server")
	upstreamPort = flag.Int("serverPort", 443, "the port on which to connect to the server")
	masqueradeAs = flag.String("masquerade", "", "masquerade host: if specified, flashlight will actually make a request to this host's IP but with a host header corresponding to the 'server' parameter")
	configDir    = flag.String("configDir", "", "directory in which to store configuration (defaults to current directory)")

	// flagsParsed is unused, this is just a trick to allow us to parse
	// command-line flags before initializing the other variables
	flagsParsed = parseFlags()

	TOMORROW             = time.Now().AddDate(0, 0, 1)
	ONE_MONTH_FROM_TODAY = time.Now().AddDate(0, 1, 0)
	ONE_YEAR_FROM_TODAY  = time.Now().AddDate(1, 0, 0)
	TEN_YEARS_FROM_TODAY = time.Now().AddDate(10, 0, 0)

	isDownstream = *upstreamHost != ""
	isUpstream   = !isDownstream

	// CloudFlare based protocol
	cp = newCloudFlareClientProtocol(*upstreamHost, *upstreamPort, *masqueradeAs)
	sp = newCloudFlareServerProtocol()

	reverseProxy = &httputil.ReverseProxy{
		Director: cp.rewrite,
		Transport: &http.Transport{
			Dial: func(network, addr string) (net.Conn, error) {
				return cp.dial(addr)
			},
		},
	}

	mitmProxy = buildMitmProxy()

	PK_FILE          = inConfigDir("proxypk.pem")
	CA_CERT_FILE     = inConfigDir("cacert.pem")
	SERVER_CERT_FILE = inConfigDir("servercert.pem")

	pk                 *keyman.PrivateKey
	caCert, serverCert *keyman.Certificate

	wg sync.WaitGroup
)

func parseFlags() bool {
	flag.Parse()
	if *help || *addr == "" {
		flag.Usage()
		os.Exit(1)
	}
	return true
}

func inConfigDir(filename string) string {
	if *configDir == "" {
		return filename
	} else {
		if _, err := os.Stat(*configDir); err != nil {
			if os.IsNotExist(err) {
				// Create config dir
				if err := os.MkdirAll(*configDir, 0755); err != nil {
					log.Fatalf("Unable to create configDir at %s: %s", *configDir, err)
				}
			}
		}
		return fmt.Sprintf("%s%c%s", *configDir, os.PathSeparator, filename)
	}
}

func main() {
	if err := initCerts(strings.Split(*addr, ":")[0]); err != nil {
		log.Fatalf("Unable to initialize certs: %s", err)
	}
	if isDownstream {
		runClient()
		buildMitmProxy()
	} else {
		runServer()
	}
	wg.Wait()
}

// runClient runs the client HTTP proxy server
func runClient() {
	// On the client, use a bunch of CPUs if necessary
	runtime.GOMAXPROCS(4)
	wg.Add(1)

	server := &http.Server{
		Addr:         *addr,
		Handler:      http.HandlerFunc(handleClient),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("About to start client (http) proxy at %s", *addr)
		if err := server.ListenAndServe(); err != nil {
			log.Fatalf("Unable to start client proxy: %s", err)
		}
		wg.Done()
	}()
}

// buildMitmProxy builds the MITM proxy that the client uses for proxying HTTPS
// requests we have to MITM these because we can't CONNECT tunnel through
// CloudFlare
func buildMitmProxy() *mitm.Proxy {
	proxy, err := mitm.NewProxy(PK_FILE, CA_CERT_FILE)
	if err != nil {
		log.Fatalf("Unable to initialize mitm proxy: %s", err)
	}
	return proxy
}

// runServer runs the server HTTPS proxy
func runServer() {
	wg.Add(1)

	server := &http.Server{
		Addr: *addr,
		Handler: &httputil.ReverseProxy{
			Director: func(req *http.Request) {
				sp.rewrite(req)
				log.Printf("Handling request for: %s", req.URL.String())
			},
		},
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("About to start server (https) proxy at %s", *addr)
		if err := server.ListenAndServeTLS(SERVER_CERT_FILE, PK_FILE); err != nil {
			// if err := server.ListenAndServe(); err != nil {
			log.Fatalf("Unable to start server proxy: %s", err)
		}
		wg.Done()
	}()
}

// handleClient handles requests from a local client (e.g. the browser)
func handleClient(resp http.ResponseWriter, req *http.Request) {
	if req.Method == "CONNECT" {
		mitmProxy.InterceptWith(resp, req, handleClientMITM)
	} else {
		reverseProxy.ServeHTTP(resp, req)
	}
}

// handleClientMITM handles requests to the client-side MITM proxy, making some
// small modifications and then delegating to doHandleClient.
func handleClientMITM(connIn net.Conn, addr string) {
	connOut, err := cp.dial(addr)
	if err != nil {
		msg := fmt.Sprintf("Unable to dial upstream proxy: %s", err)
		respondBadGateway(connIn, msg)
	}
	serverConn := httputil.NewServerConn(connIn, nil)
	go func() {
		// Read each request
		for {
			req, err := serverConn.Read()
			if err != nil {
				if err != io.EOF {
					msg := fmt.Sprintf("Unable to read request: %s", err)
					connOut.Close()
					respondBadGateway(connIn, msg)
				}
				return
			}

			req.URL.Scheme = "https"
			req.URL.Host = strings.Split(addr, ":")[0]

			// Rewrite the request
			cp.rewrite(req)

			// Write out the request
			req.Write(connOut)

			// Pipe data
			pipe(connIn, connOut)
		}
	}()
}

func hostIncludingPort(req *http.Request) (host string) {
	host = req.Host
	if !strings.Contains(host, ":") {
		host = host + ":443"
	}
	return
}

func respondBadGateway(connIn net.Conn, msg string) {
	connIn.Write([]byte(fmt.Sprintf("HTTP/1.1 502 Bad Gateway: %s", msg)))
	connIn.Close()
}

func pipe(connIn net.Conn, connOut net.Conn) {
	go func() {
		defer connIn.Close()
		io.Copy(connOut, connIn)
	}()
	go func() {
		defer connOut.Close()
		io.Copy(connIn, connOut)
	}()
}

// initCerts initializes a private key and certificates, used both for the
// server HTTPS proxy and the client MITM proxy.  Both types of proxy have a CA
// certificate.  The server proxy also gets a server certificate signed by that
// CA.  When running as a client proxy, any newly generated CA certificate is
// added to the current user's trust store (e.g. keychain) as a trusted root.
func initCerts(host string) (err error) {
	if pk, err = keyman.LoadPKFromFile(PK_FILE); err != nil {
		log.Printf("Creating new PK at: %s", PK_FILE)
		if pk, err = keyman.GeneratePK(2048); err != nil {
			return
		}
		if err = pk.WriteToFile(PK_FILE); err != nil {
			return
		}
	}

	caCert, err = keyman.LoadCertificateFromFile(CA_CERT_FILE)
	if err != nil || caCert.X509().NotAfter.Before(ONE_MONTH_FROM_TODAY) {
		log.Printf("Creating new self-signed CA cert at: %s", CA_CERT_FILE)
		if caCert, err = certificateFor("Lantern", TEN_YEARS_FROM_TODAY, true, nil); err != nil {
			return
		}
		if err = caCert.WriteToFile(CA_CERT_FILE); err != nil {
			return
		}
		if isDownstream {
			log.Println("Adding issuing cert to user trust store as trusted root")
			if err = caCert.AddAsTrustedRoot(); err != nil {
				return
			}
		}
	}

	if isUpstream {
		serverCert, err = keyman.LoadCertificateFromFile(SERVER_CERT_FILE)
		if err != nil || caCert.X509().NotAfter.Before(ONE_MONTH_FROM_TODAY) {
			log.Println("Creating new server cert at: %s", SERVER_CERT_FILE)
			if serverCert, err = certificateFor(host, ONE_YEAR_FROM_TODAY, true, caCert); err != nil {
				return
			}
			if err = serverCert.WriteToFile(SERVER_CERT_FILE); err != nil {
				return
			}
		}
	}
	return
}

// certificateFor generates a certificate for a given name, signed by the given
// issuer.  If no issuer is specified, the generated certificate is
// self-signed.
func certificateFor(
	name string,
	validUntil time.Time,
	isCA bool,
	issuer *keyman.Certificate) (cert *keyman.Certificate, err error) {
	template := &x509.Certificate{
		SerialNumber: new(big.Int).SetInt64(int64(time.Now().Nanosecond())),
		Subject: pkix.Name{
			Organization: []string{"Lantern"},
			CommonName:   name,
		},
		NotBefore: time.Now().AddDate(0, -1, 0),
		NotAfter:  validUntil,

		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
	}
	if issuer == nil {
		if isCA {
			template.KeyUsage = template.KeyUsage | x509.KeyUsageCertSign
		}
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		template.IsCA = true
	}
	cert, err = pk.Certificate(template, issuer)
	return
}
