/*
MIT License

Copyright (c) 2018 Victor Springer

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

package cache

import (
	"bufio"
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
)

// Response is the cached response data structure.
type Response struct {
	// Value is the cached response value.
	Value []byte

	// Header is the cached response header.
	Header http.Header

	// Expiration is the cached response expiration date.
	Expiration time.Time

	// LastAccess is the last date a cached response was accessed.
	// Used by LRU and MRU algorithms.
	LastAccess time.Time

	// Frequency is the count of times a cached response is accessed.
	// Used for LFU and MFU algorithms.
	Frequency int
}

type Path struct {
	Method   string
	Path     string
	Duration time.Duration
}

func (p Path) PathName() string {
	return fmt.Sprintf("%s %s", p.Method, p.Path)
}

// Client data structure for HTTP cache middleware.
type Client struct {
	adapter    Adapter
	refreshKey string
	methods    []string
	paths      []Path
}

type bodyDumpResponseWriter struct {
	io.Writer
	http.ResponseWriter
	statusCode int
}

func (w *bodyDumpResponseWriter) WriteHeader(code int) {
	w.statusCode = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *bodyDumpResponseWriter) Write(b []byte) (int, error) {
	return w.Writer.Write(b)
}

func (w *bodyDumpResponseWriter) Flush() {
	w.ResponseWriter.(http.Flusher).Flush()
}

func (w *bodyDumpResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.ResponseWriter.(http.Hijacker).Hijack()
}

// ClientOption is used to set Client settings.
type ClientOption func(c *Client) error

// Adapter interface for HTTP cache middleware client.
type Adapter interface {
	// Get retrieves the cached response by a given key. It also
	// returns true or false, whether it exists or not.
	Get(key uint64) ([]byte, bool)

	// Set caches a response for a given key until an expiration date.
	Set(key uint64, response []byte, expiration time.Time) error

	// Release frees cache for a given key.
	Release(key uint64) error
}

// Middleware is the HTTP cache middleware handler.
func (client *Client) Middleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			expiration := client.pathExpirationTime(getPathName(c))
			if expiration == nil {
				err := next(c)
				if err != nil {
					c.Error(err)
				}

				return nil
			}

			if client.cacheableMethod(c.Request().Method) {
				sortURLParams(c.Request().URL)
				key := generateKey(c.Request().Method, c.Request().URL.String())
				if c.Request().Method == http.MethodPost && c.Request().Body != nil {
					body, err := io.ReadAll(c.Request().Body)
					defer c.Request().Body.Close()
					if err != nil {
						err := next(c)
						if err != nil {
							c.Error(err)
						}

						return nil
					}
					reader := io.NopCloser(bytes.NewBuffer(body))
					key = generateKeyWithBody(c.Request().URL.String(), body)
					c.Request().Body = reader
				}

				params := c.Request().URL.Query()
				if _, ok := params[client.refreshKey]; ok {
					delete(params, client.refreshKey)

					c.Request().URL.RawQuery = params.Encode()
					key = generateKey(c.Request().Method, c.Request().URL.String())

					err := client.adapter.Release(key)
					if err != nil {
						c.Error(err)

						return nil
					}
				} else {
					b, ok := client.adapter.Get(key)
					response := BytesToResponse(b)
					if ok {
						if response.Expiration.After(time.Now()) {
							// update cache details about last access and usage frequency - only for in-memory cache
							response.LastAccess = time.Now()
							response.Frequency++
							err := client.adapter.Set(key, response.Bytes(), response.Expiration)
							if err != nil {
								c.Error(err)

								return nil
							}

							for k, v := range response.Header {
								c.Response().Header().Set(k, strings.Join(v, ","))
							}
							// TODO: handle initial code e.g. 201, 202, they are possible here as well
							c.Response().WriteHeader(http.StatusOK)
							_, err = c.Response().Write(response.Value)
							if err != nil {
								c.Error(err)

								return nil
							}

							return nil
						}

						err := client.adapter.Release(key)
						if err != nil {
							return err
						}
					}
				}

				resBody := new(bytes.Buffer)
				mw := io.MultiWriter(c.Response().Writer, resBody)
				writer := &bodyDumpResponseWriter{Writer: mw, ResponseWriter: c.Response().Writer}
				c.Response().Writer = writer
				if err := next(c); err != nil {
					c.Error(err)
				}

				statusCode := writer.statusCode
				value := resBody.Bytes()
				if statusCode < 400 {
					now := time.Now()

					var ttl time.Duration
					if expiration == nil {
						// there is no cache for this endpoint
						if err := next(c); err != nil {
							c.Error(err)
						}

						return nil
					}

					// set cache for particular endpoint
					ttl = *expiration

					response := Response{
						Value:      value,
						Header:     writer.Header(),
						Expiration: now.Add(ttl),
						LastAccess: now,
						Frequency:  1,
					}
					if err := client.adapter.Set(key, response.Bytes(), response.Expiration); err != nil {
						c.Error(err)

						return nil
					}
				}

				return nil
			}
			if err := next(c); err != nil {
				c.Error(err)
			}
			return nil
		}
	}
}

func (client *Client) cacheableMethod(method string) bool {
	for _, m := range client.methods {
		if method == m {
			return true
		}
	}
	return false
}

// "0" means no cache
func (client *Client) pathExpirationTime(path string) *time.Duration {
	for _, p := range client.paths {
		// e.g. GET /v1/markets/:marketCode/questions == POST /v1/markets/:marketCode/questions
		if strings.Contains(path, p.PathName()) {
			return &p.Duration
		}
	}

	return nil
}

func getPathName(c echo.Context) string {
	if c.Path() != "" {
		// get route from context if exists e.g. /v2/questions/:id
		return fmt.Sprintf("%s %s", c.Request().Method, c.Path())
	}

	return fmt.Sprintf("%s %s", c.Request().Method, c.Request().URL.Path)
}

// BytesToResponse converts bytes array into Response data structure.
func BytesToResponse(b []byte) Response {
	var r Response
	dec := gob.NewDecoder(bytes.NewReader(b))
	err := dec.Decode(&r)
	if err != nil {
		return Response{}
	}

	return r
}

// Bytes converts Response data structure into bytes array.
func (r Response) Bytes() []byte {
	var b bytes.Buffer
	enc := gob.NewEncoder(&b)
	err := enc.Encode(&r)
	if err != nil {
		return nil
	}

	return b.Bytes()
}

func sortURLParams(URL *url.URL) {
	params := URL.Query()
	for _, param := range params {
		sort.Slice(param, func(i, j int) bool {
			return param[i] < param[j]
		})
	}
	URL.RawQuery = params.Encode()
}

// KeyAsString can be used by adapters to convert the cache key from uint64 to string.
func KeyAsString(key uint64) string {
	return strconv.FormatUint(key, 36)
}

func generateKey(method, URL string) uint64 {
	hash := fnv.New64a()
	hash.Write([]byte(method + URL))

	return hash.Sum64()
}

func generateKeyWithBody(URL string, body []byte) uint64 {
	hash := fnv.New64a()
	body = append([]byte(URL), body...)
	hash.Write(body)

	return hash.Sum64()
}

// NewClient initializes the cache HTTP middleware client with the given
// options.
func NewClient(opts ...ClientOption) (*Client, error) {
	c := &Client{}

	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, err
		}
	}

	if c.adapter == nil {
		return nil, errors.New("cache client adapter is not set")
	}
	if len(c.paths) < 1 {
		return nil, errors.New("at least one path is required")
	}
	if c.methods == nil {
		c.methods = []string{http.MethodGet}
	}

	return c, nil
}

// ClientWithAdapter sets the adapter type for the HTTP cache
// middleware client.
func ClientWithAdapter(a Adapter) ClientOption {
	return func(c *Client) error {
		c.adapter = a
		return nil
	}
}

// ClientWithRefreshKey sets the parameter key used to free a request
// cached response. Optional setting.
func ClientWithRefreshKey(refreshKey string) ClientOption {
	return func(c *Client) error {
		c.refreshKey = refreshKey
		return nil
	}
}

// ClientWithMethods sets the acceptable HTTP methods to be cached.
// Optional setting. If not set, default is "GET".
func ClientWithMethods(methods []string) ClientOption {
	return func(c *Client) error {
		for _, method := range methods {
			if method != http.MethodGet && method != http.MethodPost {
				return fmt.Errorf("invalid method %s", method)
			}
		}
		c.methods = methods
		return nil
	}
}

// ClientWithPaths sets the custom timeouts for particular endpoints.
// Optional setting.
func ClientWithPaths(values []Path) ClientOption {
	return func(c *Client) error {
		c.paths = values
		return nil
	}
}
