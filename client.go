package cosmosdb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/pkg/errors"
)

const (
	HEADER_XDATE        = "X-Ms-Date"
	HEADER_AUTH         = "Authorization"
	HEADER_VER          = "X-Ms-Version"
	HEADER_CONTYPE      = "Content-Type"
	HEADER_CONLEN       = "Content-Length"
	HEADER_IS_QUERY     = "X-Ms-Documentdb-Isquery"
	HEADER_UPSERT       = "X-Ms-Documentdb-Is-Upsert"
	HEADER_CONTINUATION = "X-Ms-Continuation"
	HEADER_IF_MATCH     = "If-Match"
	HEADER_CHARGE       = "X-Ms-Request-Charge"

	HEADER_CROSSPARTITION = "x-ms-documentdb-query-enablecrosspartition"
	HEADER_PARTITIONKEY   = "x-ms-documentdb-partitionkey"
)

var (
	errRetry              = errors.New("retry")
	IgnoreContext         bool
	ErrPreconditionFailed = errors.New("precondition failed")
	ResponseHook          func(ctx context.Context, method string, headers map[string][]string)
)

type Config struct {
	MasterKey  string
	MaxRetries int
}

type Client struct {
	Url    string
	Config Config
	Client *http.Client
}

// New makes a new client to communicate to a cosmosdb instance.
// If no http.Client is provided it defaults to the http.DefaultClient
func New(url string, cfg Config, cl *http.Client) *Client {
	client := &Client{
		Url:    strings.Trim(url, "/"),
		Config: cfg,
		Client: cl,
	}

	if client.Client == nil {
		client.Client = http.DefaultClient
	}

	return client
}

type RequestOptions map[RequestOption]string
type RequestOption string

var (
	ReqOpAllowCrossPartition = RequestOption("x-ms-documentdb-query-enablecrosspartition")
	ReqOpPartitionKey        = RequestOption(HEADER_PARTITIONKEY)
)

func (c *Client) get(ctx context.Context, link string, ret interface{}, headers map[string]string) error {
	_, err := c.method(ctx, "GET", link, ret, nil, headers)
	return err
}

// Create resource
func (c *Client) create(ctx context.Context, link string, body, ret interface{}, headers map[string]string) error {
	data, err := stringify(body)
	if err != nil {
		return err
	}
	buf := bytes.NewBuffer(data)
	fmt.Printf("Request body: \n%s\n", data)

	fmt.Printf("Will call c.method\n")
	_, err = c.method(ctx, "POST", link, ret, buf, headers)
	return err
}

func defaultHeaders(method, link, key string) (map[string]string, error) {
	h := map[string]string{}
	h[HEADER_XDATE] = time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT")
	h[HEADER_VER] = "2017-02-22" // TODO: move to package level
	//h[HEADER_CROSSPARTITION] = "true"

	sign, err := signedPayload(method, link, h[HEADER_XDATE], key)
	if err != nil {
		return h, err
	}

	h[HEADER_AUTH] = authHeader(sign)

	fmt.Printf("Auth header: %s\n", h[HEADER_AUTH])

	return h, nil
}

func (c *Client) method(ctx context.Context, method, link string, ret interface{}, body io.Reader, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequest(method, path(c.Url, link), body)
	if err != nil {
		fmt.Println(err)
		return nil, err
	}
	fmt.Printf("Will call: %s\n", req.URL)
	//r := ResourceRequest(link, req)

	defaultHeaders, err := defaultHeaders(method, link, c.Config.MasterKey)
	if err != nil {
		fmt.Println(err)
		return nil, err
	}

	if headers == nil {
		headers = map[string]string{}
	}
	for k, v := range defaultHeaders {
		// insert if not already present
		headers[k] = v
	}

	for k, v := range headers {
		req.Header.Add(k, v)
	}

	fmt.Printf("Headers: %s\n", req.Header)

	return c.do(ctx, req, ret)
}

func retriable(code int) bool {
	return code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable
}

// Request Error
type RequestError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Implement Error function
func (e RequestError) Error() string {
	return fmt.Sprintf("%v, %v", e.Code, e.Message)
}

func (c *Client) checkResponse(ctx context.Context, retryCount int, resp *http.Response) error {
	if retriable(resp.StatusCode) {
		if retryCount < c.Config.MaxRetries {
			delay := backoffDelay(retryCount)
			t := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				t.Stop()
				return ctx.Err()
			case <-t.C:
				return errRetry
			}
		}
	}
	if resp.StatusCode == http.StatusPreconditionFailed {
		return ErrPreconditionFailed
	}
	if resp.StatusCode >= 300 {
		err := &RequestError{}
		readJson(resp.Body, &err)
		return err
	}

	return nil
}

// Private Do function, DRY
func (c *Client) do(ctx context.Context, r *http.Request, data interface{}) (*http.Response, error) {
	cli := c.Client
	if cli == nil {
		cli = http.DefaultClient
	}
	if !IgnoreContext {
		r = r.WithContext(ctx)
	}
	// save body to be able to retry the request
	b := []byte{}
	if r.Body != nil {
		var err error
		b, err = ioutil.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
	}
	retryCount := 0
	for {
		r.Body = ioutil.NopCloser(bytes.NewReader(b))
		fmt.Printf("Executing request\n")
		resp, err := cli.Do(r)
		if err != nil {
			return nil, err
		}
		if ResponseHook != nil {
			ResponseHook(ctx, r.Method, resp.Header)
		}
		err = c.checkResponse(ctx, retryCount, resp)
		if err == errRetry {
			resp.Body.Close()
			retryCount++
			continue
		}
		defer resp.Body.Close()

		if err != nil {
			return resp, err
		}

		if data == nil {
			return resp, nil
		}
		return resp, readJson(resp.Body, data)
	}
}

func backoffDelay(retryCount int) time.Duration {
	minTime := 300

	if retryCount > 13 {
		retryCount = 13
	} else if retryCount > 8 {
		retryCount = 8
	}

	delay := (1 << uint(retryCount)) * (rand.Intn(minTime) + minTime)
	return time.Duration(delay) * time.Millisecond
}

// Generate link
func path(url string, args ...string) (link string) {
	args = append([]string{url}, args...)
	link = strings.Join(args, "/")
	return
}

// Read json response to given interface(struct, map, ..)
func readJson(reader io.Reader, data interface{}) error {
	return json.NewDecoder(reader).Decode(data)
}

// Stringify body data
func stringify(body interface{}) (bt []byte, err error) {
	switch t := body.(type) {
	case string:
		bt = []byte(t)
	case []byte:
		bt = t
	default:
		bt, err = json.Marshal(t)
	}
	return
}
