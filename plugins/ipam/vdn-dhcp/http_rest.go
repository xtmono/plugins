package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/coreos/go-systemd/journal"
)

type RestClient struct {
	baseUrl    *url.URL
	user       string
	password   string
	httpClient *http.Client
}

func NewRestClient(path, user, password string) *RestClient {
	baseUrl, err := url.Parse(path)
	if err != nil {
		return nil
	}
	return &RestClient{
		baseUrl:    baseUrl,
		user:       user,
		password:   password,
		httpClient: &http.Client{}}
}

func (c *RestClient) Get(path string, resp interface{}) error {
	req, err := c.newRequest("GET", path, nil)
	if err != nil {
		return err
	}
	r, err := c.do(req, resp)
	if err == nil && r.StatusCode != 200 {
		err = fmt.Errorf("rest server response error: %s", r.Status)
	}
	return err
}

func (c *RestClient) Post(path string, body interface{}, resp interface{}) error {
	req, err := c.newRequest("POST", path, body)
	if err != nil {
		return err
	}
	r, err := c.do(req, resp)
	if err == nil && r.StatusCode != 200 {
		err = fmt.Errorf("rest server response error: %s", r.Status)
	}
	return err
}

func (c *RestClient) Put(path string, body interface{}, resp interface{}) error {
	req, err := c.newRequest("PUT", path, body)
	if err != nil {
		return err
	}
	r, err := c.do(req, resp)
	if err == nil && r.StatusCode != 200 {
		err = fmt.Errorf("rest server response error: %s", r.Status)
	}
	return err
}

func (c *RestClient) Delete(path string) error {
	req, err := c.newRequest("DELETE", path, nil)
	if err != nil {
		return err
	}
	r, err := c.do(req, nil)
	if err == nil && r.StatusCode != 200 {
		err = fmt.Errorf("rest server response error: %s", r.Status)
	}
	return err
}

func (c *RestClient) newRequest(method, path string, body interface{}) (*http.Request, error) {
	reqUrl := c.baseUrl.ResolveReference(&url.URL{Path: url.PathEscape(path)})

	var buf io.ReadWriter
	if body != nil {
		buf = new(bytes.Buffer)
		err := json.NewEncoder(buf).Encode(body)
		if err != nil {
			return nil, err
		}
	}

	journal.Print(journal.PriInfo, "newRequest: %s", reqUrl.String())
	req, err := http.NewRequest(method, reqUrl.String(), buf)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "CNI-Plugin")
	req.SetBasicAuth(c.user, c.password)
	return req, nil
}

func (c *RestClient) do(req *http.Request, v interface{}) (*http.Response, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if v != nil {
		err = json.NewDecoder(resp.Body).Decode(v)
	}
	return resp, err
}
