package gospdyquic

import (
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"

	"github.com/devsisters/goquic"
)

type QuicRoundTripper struct {
	conns map[string]*goquic.Conn
}

type badStringError struct {
	what string
	str  string
}

func NewRoundTripper() *QuicRoundTripper {
	return &QuicRoundTripper{
		conns: make(map[string]*goquic.Conn),
	}
}

func (e *badStringError) Error() string { return fmt.Sprintf("%s %q", e.what, e.str) }

// XXX: goquic.Initialize() should be called by user first.

func (q *QuicRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method != "GET" {
		return nil, errors.New("non-GET request is not supported yet. Sorry.")
		// TODO(hodduc): POST / HEAD / PUT support
	}

	conn, exists := q.conns[request.Host]
	if !exists {
		conn_new, err := goquic.Dial("udp4", request.Host)
		if err != nil {
			panic(err)
		}

		q.conns[request.Host] = conn_new
		conn = conn_new
	}

	st := conn.CreateStream()

	header := make(http.Header)
	for k, v := range request.Header {
		for _, vv := range v {
			header.Add(k, vv)
		}
	}
	header.Set(":host", request.Host)
	header.Set(":version", request.Proto)
	header.Set(":method", request.Method)
	header.Set(":path", request.URL.RequestURI())
	header.Set(":scheme", request.URL.Scheme)

	if request.Method == "GET" {
		st.WriteHeader(header, true)
	}

	recvHeader, err := st.ReadHeader()
	if err != nil {
		return nil, err
	}

	resp := &http.Response{}
	resp.Status = recvHeader.Get(":status")
	f := strings.SplitN(resp.Status, " ", 3)
	if len(f) < 2 {
		return nil, &badStringError{"malformed HTTP response", resp.Status}
	}
	resp.StatusCode, err = strconv.Atoi(f[0])
	if err != nil {
		return nil, &badStringError{"malformed HTTP status code", f[1]}
	}

	resp.Proto = recvHeader.Get(":version")
	var ok bool
	if resp.ProtoMajor, resp.ProtoMinor, ok = http.ParseHTTPVersion(resp.Proto); !ok {
		return nil, &badStringError{"malformed HTTP version", resp.Proto}
	}

	resp.Header = recvHeader

	resp.ContentLength, err = strconv.ParseInt(recvHeader.Get("content-length"), 10, 64)
	if err != nil {
		resp.ContentLength = -1
	}
	resp.Request = request
	resp.Body = ioutil.NopCloser(st)

	return resp, nil
}
