package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

type RestClient struct {
	baseUrl    *url.URL
	user       string
	password   string
	httpClient *http.Client
}

func NewRestClient(user, password, path string) *RestClient {
	return &RestClient{
		baseUrl:    &url.URL{Path: path},
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
	if r.StatusCode != 200 {
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
	if r.StatusCode != 200 {
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
	if r.StatusCode != 200 {
		err = fmt.Errorf("rest server response error: %s", r.Status)
	}
	return err
}

func (c *RestClient) Delete(path string) error {
	req, err := c.newRequest("GET", path, nil)
	if err != nil {
		return err
	}
	r, err := c.do(req, nil)
	if r.StatusCode != 200 {
		err = fmt.Errorf("rest server response error: %s", r.Status)
	}
	return err
}

func (c *RestClient) newRequest(method, path string, body interface{}) (*http.Request, error) {
	rel := &url.URL{Path: path}
	u := c.baseUrl.ResolveReference(rel)
	var buf io.ReadWriter
	if body != nil {
		buf = new(bytes.Buffer)
		err := json.NewEncoder(buf).Encode(body)
		if err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequest(method, u.String(), buf)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Rest client")
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
