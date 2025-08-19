//  Copyright (c) 2025 dingodb.com, Inc. All Rights Reserved
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http:www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

package util

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"dingospeed/pkg/common"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	"dingospeed/pkg/prom"

	"github.com/avast/retry-go"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

var ProxyIsAvailable = true

// 全局缓存http.Transport（按代理地址区分，避免重复创建连接池）
var transportMap = sync.Map{} // key: proxyAddr, value: *http.Transport

func RetryRequest(f func() (*common.Response, error)) (*common.Response, error) {
	var resp *common.Response
	err := retry.Do(
		func() error {
			var err error
			resp, err = f()
			return err
		},
		retry.Delay(time.Duration(config.SysConfig.Retry.Delay)*time.Second),
		retry.Attempts(config.SysConfig.Retry.Attempts),
		retry.DelayType(retry.FixedDelay),
	)
	return resp, err
}

func NewHTTPClient(timeout time.Duration) (*http.Client, error) {
	client := &http.Client{Timeout: timeout}
	return client, nil
}

// NewHTTPClientWithProxy 创建HTTP客户端，复用按代理地址缓存的Transport
func NewHTTPClientWithProxy(timeout time.Duration) (*http.Client, error) {
	// 1. 获取代理地址（从配置中读取）
	proxyAddr := config.SysConfig.GetHttpProxy()

	// 2. 从缓存获取或创建对应的Transport
	var transport *http.Transport
	if proxyAddr == "" {
		// 无代理时使用默认Transport
		transport = http.DefaultTransport.(*http.Transport).Clone() // 克隆默认配置，避免修改全局默认值
	} else {
		// 有代理时，从缓存加载或创建Transport
		val, ok := transportMap.Load(proxyAddr)
		if !ok {
			// 解析代理地址
			proxyURL, err := url.Parse(proxyAddr)
			if err != nil {
				return nil, fmt.Errorf("代理地址解析失败: %v", err)
			}
			// 创建带连接池优化的Transport
			newTransport := &http.Transport{
				Proxy: http.ProxyURL(proxyURL),
				DialContext: (&net.Dialer{
					Timeout:   30 * time.Second, // 拨号超时
					KeepAlive: 30 * time.Second, // 长连接保活时间
				}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second, // TLS握手超时
				ForceAttemptHTTP2:     false,            // 禁用HTTP/2（根据需求调整）
				ResponseHeaderTimeout: 10 * time.Second, // 响应头超时
				IdleConnTimeout:       90 * time.Second, // 空闲连接超时
				MaxIdleConns:          100,              // 最大空闲连接数（全局）
				MaxIdleConnsPerHost:   10,               // 每个主机的最大空闲连接数
				MaxConnsPerHost:       100,              // 每个主机的最大连接数（限制并发）
			}
			// 存入缓存（LoadOrStore确保并发安全）
			val, _ = transportMap.LoadOrStore(proxyAddr, newTransport)
		}
		// 从缓存中取出Transport
		transport = val.(*http.Transport)
	}

	// 3. 创建新的HTTP客户端（轻量对象，每次创建新实例避免状态污染）
	client := &http.Client{
		Timeout:   timeout,   // 客户端整体超时（覆盖连接、读取等阶段）
		Transport: transport, // 复用缓存的Transport（连接池核心）
	}

	zap.S().Debugf("已初始化HTTP客户端（代理: %s, 超时: %v）", proxyAddr, timeout)
	return client, nil
}

func constructClient(timeout time.Duration) (string, *http.Client, error) {
	var (
		domain string
		client *http.Client
		err    error
	)
	if !ProxyIsAvailable && config.SysConfig.DynamicProxy.Enabled {
		domain = config.SysConfig.GetBpHFURLBase()
		client, err = NewHTTPClient(timeout)
	} else {
		domain = config.SysConfig.GetHFURLBase()
		client, err = NewHTTPClientWithProxy(timeout)
	}
	return domain, client, err
}

func Head(requestUri string, headers map[string]string, timeout time.Duration) (*common.Response, error) {
	domain, client, err := constructClient(timeout)
	if err != nil {
		return nil, fmt.Errorf("construct http client err: %v", err)
	}
	requestURL := fmt.Sprintf("%s%s", domain, requestUri)
	return doHead(client, requestURL, headers)
}

func doHead(client *http.Client, targetURL string, headers map[string]string) (*common.Response, error) {
	req, err := http.NewRequest("HEAD", targetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建HEAD请求失败: %v", err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		zap.S().Warnf("URL请求失败: %s, 错误: %v", targetURL, err)
		return nil, fmt.Errorf("执行HEAD请求失败: %v", err)
	}
	defer func() {
		if r := recover(); r != nil {
			zap.S().Errorf("关闭响应体资源时出现异常: %v", r)
		}
		resp.Body.Close()
	}()
	respHeaders := make(map[string]interface{})
	for key, values := range resp.Header {
		respHeaders[key] = values
	}
	return &common.Response{
		StatusCode: resp.StatusCode,
		Headers:    respHeaders,
	}, nil
}

func Get(requestUri string, headers map[string]string, timeout time.Duration) (*common.Response, error) {
	domain, client, err := constructClient(timeout)
	if err != nil {
		return nil, fmt.Errorf("construct http client err: %v", err)
	}
	requestURL := fmt.Sprintf("%s%s", domain, requestUri)
	return doGet(client, requestURL, headers)
}

func doGet(client *http.Client, targetURL string, headers map[string]string) (*common.Response, error) {
	req, err := http.NewRequest("GET", targetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建GET请求失败: %v", err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		zap.S().Warnf("URL请求失败: %s, 错误: %v", targetURL, err)
		return nil, fmt.Errorf("执行GET请求失败: %v", err)
	}

	defer func() {
		if r := recover(); r != nil {
			zap.S().Errorf("关闭响应体资源时出现异常: %v", r)
		}
		resp.Body.Close()
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应体失败: %v", err)
	}

	respHeaders := make(map[string]interface{})
	for key, values := range resp.Header {
		respHeaders[key] = values
	}

	return &common.Response{
		StatusCode: resp.StatusCode,
		Headers:    respHeaders,
		Body:       body,
	}, nil
}

func GetStream(domain, uri string, headers map[string]string, timeout time.Duration, f func(r *http.Response) error) error {
	var (
		client *http.Client
		err    error
	)
	if IsInnerDomain(domain) {
		client, err = NewHTTPClient(timeout)
		headers[consts.RequestSourceInner] = Itoa(1)
	} else {
		domain, client, err = constructClient(timeout)
	}
	if err != nil {
		return fmt.Errorf("construct http client err: %v", err)
	}
	requestURL := fmt.Sprintf("%s%s", domain, uri)
	return doGetStream(client, requestURL, headers, timeout, f)
}

func doGetStream(client *http.Client, targetURL string, headers map[string]string, timeout time.Duration, f func(r *http.Response) error) error {
	escapedURL := strings.ReplaceAll(targetURL, "#", "%23")
	req, err := http.NewRequest("GET", escapedURL, nil)
	if err != nil {
		return fmt.Errorf("创建GET请求失败: %v", err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respHeaders := make(map[string]interface{})
	for key, value := range resp.Header {
		respHeaders[key] = value
	}
	return f(resp)
}

func Post(requestUri string, contentType string, data []byte, headers map[string]string) (*common.Response, error) {
	domain, client, err := constructClient(config.SysConfig.GetReqTimeOut())
	if err != nil {
		return nil, fmt.Errorf("construct http client err: %v", err)
	}
	requestURL := fmt.Sprintf("%s%s", domain, requestUri)
	return doPost(client, requestURL, contentType, data, headers)
}

func doPost(client *http.Client, targetURL string, contentType string, data []byte, headers map[string]string) (*common.Response, error) {
	req, err := http.NewRequest("POST", targetURL, bytes.NewBuffer(data))
	if err != nil {
		return nil, fmt.Errorf("创建POST请求失败: %v", err)
	}

	req.Header.Set("Content-Type", contentType)
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	resp, err := client.Do(req)
	if err != nil {
		zap.S().Warnf("URL请求失败: %s, 错误: %v", targetURL, err)
		return nil, fmt.Errorf("执行POST请求失败: %v", err)
	}

	defer func() {
		if r := recover(); r != nil {
			zap.S().Errorf("关闭响应体资源时出现异常: %v", r)
		}
		resp.Body.Close()
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应体失败: %v", err)
	}

	respHeaders := make(map[string]interface{})
	for key, values := range resp.Header {
		respHeaders[key] = values
	}

	return &common.Response{
		StatusCode: resp.StatusCode,
		Headers:    respHeaders,
		Body:       body,
	}, nil
}

func ResponseStream(c echo.Context, fileName string, headers map[string]string, content <-chan []byte) error {
	c.Response().Header().Set("Content-Type", "text/event-stream")
	c.Response().Header().Set("Cache-Control", "no-cache")
	c.Response().Header().Set("Connection", "keep-alive")
	for k, v := range headers {
		c.Response().Header().Set(k, v)
	}
	c.Response().WriteHeader(http.StatusOK)
	flusher, ok := c.Response().Writer.(http.Flusher)
	if !ok {
		return c.String(http.StatusInternalServerError, "Streaming unsupported!")
	}
	for {
		select {
		case b, ok := <-content:
			if !ok {
				zap.S().Infof("ResponseStream complete, %s", fileName)
				return nil
			}
			if len(b) > 0 {
				if _, err := c.Response().Write(b); err != nil {
					zap.S().Warnf("ResponseStream write err,file:%s,%v", fileName, err)
					return ErrorProxyTimeout(c)
				}
				if config.SysConfig.EnableMetric() {
					// 原子性地更新响应总数
					source := Itoa(c.Get(consts.PromSource))
					orgRepo := Itoa(c.Get(consts.PromOrgRepo))
					prom.PromRequestByteCounter(prom.RequestResponseByte, source, int64(len(b)), orgRepo)
				}
			}
			flusher.Flush()
		}
	}
}

func ForwardRequest(originalReq *http.Request, timeout time.Duration) (*common.Response, error) {
	domain, client, err := constructClient(timeout)
	if err != nil {
		return nil, fmt.Errorf("construct http client err: %v", err)
	}
	targetURL := fmt.Sprintf("%s%s", domain, originalReq.URL.RequestURI())
	proxyReq, err := http.NewRequest(originalReq.Method, targetURL, originalReq.Body)
	if err != nil {
		return nil, fmt.Errorf("创建转发请求失败: %v", err)
	}
	for key, values := range originalReq.Header {
		for _, value := range values {
			proxyReq.Header.Add(key, value)
		}
	}
	resp, err := client.Do(proxyReq)
	if err != nil {
		zap.S().Warnf("转发请求失败: %s, 错误: %v", targetURL, err)
		return nil, fmt.Errorf("执行转发请求失败: %v", err)
	}
	defer func() {
		if r := recover(); r != nil {
			zap.S().Errorf("关闭响应体资源时出现异常: %v", r)
		}
		resp.Body.Close()
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应体失败: %v", err)
	}
	respHeaders := make(map[string]interface{})
	for key, values := range resp.Header {
		respHeaders[key] = values
	}
	return &common.Response{
		StatusCode: resp.StatusCode,
		Headers:    respHeaders,
		Body:       body,
	}, nil
}

func IsInnerDomain(url string) bool {
	return !strings.Contains(url, consts.Huggingface) && !strings.Contains(url, consts.Hfmirror)
}
