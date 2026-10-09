package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func researchServerRequest(base, command string, body any) (any, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("--server must be a plain loopback HTTP origin")
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return nil, errors.New("--server must use a numeric loopback address")
	}
	origin := "http://" + u.Host
	method := "GET"
	var input io.Reader
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			return nil, e
		}
		input = bytes.NewReader(b)
		method = "POST"
	}
	req, err := http.NewRequest(method, origin+"/api/"+command, input)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Origin", origin)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 190 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("research server redirects are disabled")
	}}
	response, err := client.Do(req)
	if err != nil {
		return nil, errors.New("research server request failed")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 8*1024*1024+1))
	if err != nil || len(data) > 8*1024*1024 {
		return nil, errors.New("research server response is incomplete or too large")
	}
	if response.StatusCode != 200 {
		var failure struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &failure) == nil && strings.TrimSpace(failure.Error) != "" {
			return nil, errors.New(failure.Error)
		}
		return nil, errors.New("research server rejected the request")
	}
	var out any
	if err = json.Unmarshal(data, &out); err != nil {
		return nil, errors.New("invalid research server response")
	}
	return out, nil
}
