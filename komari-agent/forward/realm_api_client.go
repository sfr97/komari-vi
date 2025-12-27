package forward

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type RealmApiClient struct {
	baseURL string
	client  *http.Client
}

func NewRealmApiClient(baseURL string) *RealmApiClient {
	return &RealmApiClient{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		client:  &http.Client{Timeout: 5 * time.Second},
	}
}

type realmApiErrorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type RealmInstance struct {
	ID     string            `json:"id"`
	Config RealmEndpointConf `json:"config"`
	Status json.RawMessage   `json:"status"`
}

type RealmEndpointConf struct {
	Listen       string          `json:"listen"`
	Remote       string          `json:"remote"`
	ExtraRemotes []string        `json:"extra_remotes"`
	Balance      *string         `json:"balance"`
	Network      json.RawMessage `json:"network"`
}

func (c *RealmApiClient) ListInstances() ([]RealmInstance, error) {
	var out []RealmInstance
	if err := c.doJSON("GET", "/instances", nil, 200, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *RealmApiClient) GetInstance(id string) (*RealmInstance, error) {
	var out RealmInstance
	if err := c.doJSON("GET", "/instances/"+url.PathEscape(id), nil, 200, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *RealmApiClient) UpsertInstance(body json.RawMessage) error {
	return c.doJSON("POST", "/instances", body, 200, nil, 201)
}

func (c *RealmApiClient) StartInstance(id string) error {
	return c.doJSON("POST", "/instances/"+url.PathEscape(id)+"/start", nil, 200, nil, 409)
}

func (c *RealmApiClient) StopInstance(id string) error {
	return c.doJSON("POST", "/instances/"+url.PathEscape(id)+"/stop", nil, 200, nil, 409)
}

func (c *RealmApiClient) DeleteInstance(id string) error {
	return c.doJSON("DELETE", "/instances/"+url.PathEscape(id), nil, 204, nil, 404)
}

func (c *RealmApiClient) GetInstanceStatsRaw(id string) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.doJSON("GET", "/instances/"+url.PathEscape(id)+"/stats", nil, 200, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func (c *RealmApiClient) GetInstanceRouteRaw(id string) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.doJSON("GET", "/instances/"+url.PathEscape(id)+"/route", nil, 200, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func (c *RealmApiClient) GetInstanceConnectionsRaw(id string, protocol string, limit int, offset int) (json.RawMessage, error) {
	q := url.Values{}
	if strings.TrimSpace(protocol) != "" {
		q.Set("protocol", protocol)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	path := "/instances/" + url.PathEscape(id) + "/connections"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}

	var raw json.RawMessage
	if err := c.doJSON("GET", path, nil, 200, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func listenPortFromAddr(addr string) (int, bool) {
	s := strings.TrimSpace(addr)
	if s == "" {
		return 0, false
	}
	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		// Try ":port" style when host omitted.
		if strings.Count(s, ":") == 0 {
			return 0, false
		}
		host, portStr, err = net.SplitHostPort(":" + strings.TrimPrefix(s, ":"))
		if err != nil {
			return 0, false
		}
		_ = host
	}
	p, err := strconv.Atoi(portStr)
	if err != nil || p <= 0 || p > 65535 {
		return 0, false
	}
	_ = host
	return p, true
}

func (c *RealmApiClient) doJSON(method string, path string, body []byte, expectStatus int, out any, allowStatuses ...int) error {
	full := c.baseURL + path
	req, err := http.NewRequest(method, full, nil)
	if err != nil {
		return err
	}
	if body != nil {
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	ok := resp.StatusCode == expectStatus
	if !ok {
		for _, s := range allowStatuses {
			if resp.StatusCode == s {
				ok = true
				break
			}
		}
	}

	b, _ := io.ReadAll(resp.Body)

	if !ok {
		var apiErr realmApiErrorResponse
		if len(b) > 0 && json.Unmarshal(b, &apiErr) == nil && apiErr.Error.Code != "" {
			return fmt.Errorf("realm api error: status=%d, code=%s, message=%s", resp.StatusCode, apiErr.Error.Code, apiErr.Error.Message)
		}
		return fmt.Errorf("realm api error: status=%d, body=%s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("decode realm api response failed: %w", err)
		}
	}
	return nil
}
