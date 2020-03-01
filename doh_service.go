package main

// https://developers.cloudflare.com/1.1.1.1/dns-over-https/wireformat/

import (
	"bytes"
	"crypto/tls"
	"io/ioutil"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/miekg/dns"
	"github.com/patrickmn/go-cache"
)

type DohError struct {
	msg string
}

func (e *DohError) Error() string {
	return e.msg
}

func newErr(msg string) error {
	return &DohError{msg}
}

const CLOUDFLARE_DNS = "1.1.1.1:53"
const CLOUDFLARE_DOH_HOST = "cloudflare-dns.com."
const CLOUDFLARE_DOH_URL = "https://cloudflare-dns.com/dns-query"

// Create HTTPS request and POST.
func makeHttpsRequest(wire []byte) (respWire []byte, err error) {
	// disable security check for client
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{Transport: tr}
	buff := bytes.NewBuffer(wire)

	resp, err := client.Post(CLOUDFLARE_DOH_URL,
		"application/dns-udpwireformat", buff)

	if err == nil {
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			return nil, newErr("HTTP error code " + resp.Status)
		}

		respBody, err := ioutil.ReadAll(resp.Body)
		if err == nil {
			return respBody, nil
		} else {
			// io: read error
			return nil, err
		}
	} else {
		// http error
		return nil, err
	}
}

type SecHandler struct {
	ServiceType string
	Host        *dns.Msg
	NameCache   *cache.Cache
}

func (s SecHandler) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	if len(r.Question) > 0 && r.Question[0].Qtype == dns.TypeA {
		// TypeA request

		if r.Question[0].Name == CLOUDFLARE_DOH_HOST {
			// Cloudflare DNS over HTTPS server name
			s.Host.SetReply(r)
			w.WriteMsg(s.Host)
		} else {
			// Other TypeA request
			requestedName := r.Question[0].Name

			if x, found := s.NameCache.Get(requestedName); found {
				// Cache hit:
				cachedMsg := x.(*dns.Msg)
				cachedMsg.SetReply(r)
				w.WriteMsg(cachedMsg)
			} else {
				// Cache miss:
				respMsg, err := s.QueryOverHTTPS(r)

				if err == nil {
					s.NameCache.SetDefault(requestedName, respMsg)
					respMsg.SetReply(r)
					w.WriteMsg(respMsg)
				} else {
					log.Printf("requested name = %s", requestedName)
					WriteErrorLog(err)
					dns.HandleFailed(w, r)
				}
			}
		}
	} else {
		// all other request: just relay
		respMsg, err := s.QueryOverHTTPS(r)

		if err == nil {
			respMsg.SetReply(r)
			w.WriteMsg(respMsg)
		} else {
			dns.HandleFailed(w, r)
		}
	}
}

func (s SecHandler) QueryOverHTTPS(r *dns.Msg) (*dns.Msg, error) {
	wire, err := r.Pack()

	if err == nil {
		resp, err := makeHttpsRequest(wire)
		if err == nil {
			// Good response then
			m := new(dns.Msg)
			err := m.Unpack(resp)
			if err == nil {
				return m, nil
			}
			return nil, newErr("Can't unpack message from wireformat.")
		}
		return nil, newErr("HTTPS Request failed.")
	}
	return nil, newErr("Can't pack message from wireformat.")
}

func getDohHostAddr() (*dns.Msg, error) {
	m := new(dns.Msg)
	m.SetQuestion(CLOUDFLARE_DOH_HOST, dns.TypeA)

	client := new(dns.Client)
	r, _, err := client.Exchange(m, CLOUDFLARE_DNS)
	return r, err
}

type SvrStopFunc func() error
type SvrErrorHandlerFunc func(err error)

func RunDNS(port int, errHandler SvrErrorHandlerFunc) (SvrStopFunc, error) {
	// get DOH host address
	h, e := getDohHostAddr()
	if e != nil {
		WriteErrorLogMsg("Failed to obtain Cloudflare's DOH server address.", e)

		// 2019.3.5. DOH 서버 주소를 가져오는데 실패하였을 경우 잠시 기다린 후
		// 다시 시도한다. (5회까지)
		// 부팅 후 서비스가 처음 시작될 때 "A socket operation was attempted to
		// an unreachable host." 오류가 발생할 경우 서비스가 중단되는것을 방지.
		for i := 1; i < 6; i++ {
			// wait some seconds
			time.Sleep(time.Second * 1)

			log.Printf("retry %d...", i)
			h, e = getDohHostAddr()

			if e != nil {
				WriteErrorLogMsg("Failed to obtain Cloudflare's DOH server address.", e)
			} else {
				break
			}
		}

		if e != nil {
			return nil, newErr("Failed to obtain Cloudflare's DOH server address. The DNS service could not be started.")
		}
	}

	handler := SecHandler{
		"UDP",
		h,
		cache.New(1*time.Hour, 10*time.Minute),
	}
	srv := new(dns.Server)
	srv.Addr = ":" + strconv.Itoa(port)
	srv.Net = "udp"
	srv.Handler = handler

	go func() {
		if err := srv.ListenAndServe(); err != nil {
			errHandler(err)
		}
	}()

	return func() error {
		if srv != nil {
			return srv.Shutdown()
		}
		return newErr("No DNS server instance.")
	}, nil
}
