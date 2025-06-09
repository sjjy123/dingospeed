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

// buildURL 构建请求URL，根据代理可用性选择主URL或备用URL
func buildURL(requestURL string) (string, error) {
	parsedURL, err := url.Parse(requestURL)
	if err != nil {
		return "", fmt.Errorf("解析请求URL失败: %w", err)
	}
	if ProxyIsAvailable || !config.SysConfig.DynamicProxy.Enabled {
		return config.SysConfig.GetHFURLBase() + parsedURL.Path, nil
	}
	return config.SysConfig.GetBpHFURLBase() + parsedURL.Path, nil
}

// NewHTTPClientWithProxy 根据代理地址、超时时间和代理可用性创建HTTP客户端
func NewHTTPClientWithProxy(proxyAddr string, timeout time.Duration, proxyIsAvailable bool) (*http.Client, error) {
	client := &http.Client{Timeout: timeout}
	if !proxyIsAvailable || proxyAddr == "" {
		if proxyAddr == "" {
			zap.S().Warnf("未配置代理，使用默认HTTP客户端")
		}
		return client, nil
	}

	proxyURL, err := url.Parse(proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("代理地址解析失败: %v", err)
	}

	transport := &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ForceAttemptHTTP2:     false,
		ResponseHeaderTimeout: 10 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	}

	client.Transport = transport
	zap.S().Warnf("已启用HTTP代理: %s", proxyAddr)
	return client, nil
}

// Head 方法用于发送带请求头的 HEAD 请求
func Head(requestURL string, headers map[string]string, timeout time.Duration) (*common.Response, error) {
	targetURL, err := buildURL(requestURL)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("HEAD", targetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建HEAD请求失败: %v", err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	client, clientErr := NewHTTPClientWithProxy(config.SysConfig.GetHttpProxy(), timeout, ProxyIsAvailable)
	if clientErr != nil {
		return nil, clientErr
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

// Get 方法用于发送带请求头的 GET 请求
func Get(requestURL string, headers map[string]string, timeout time.Duration) (*common.Response, error) {
	targetURL, err := buildURL(requestURL)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("GET", targetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建GET请求失败: %v", err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	client, clientErr := NewHTTPClientWithProxy(config.SysConfig.GetHttpProxy(), timeout, ProxyIsAvailable)
	if clientErr != nil {
		return nil, clientErr
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

// GetStream 方法用于发送带请求头的 GET 请求并流式处理响应
func GetStream(requestURL string, headers map[string]string, timeout time.Duration, f func(r *http.Response)) error {
	targetURL, err := buildURL(requestURL)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("GET", targetURL, nil)
	if err != nil {
		return fmt.Errorf("创建GET请求失败: %v", err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	client, clientErr := NewHTTPClientWithProxy(config.SysConfig.GetHttpProxy(), timeout, ProxyIsAvailable)
	if clientErr != nil {
		return clientErr
	}

	resp, err := client.Do(req)
	if err != nil {
		zap.S().Warnf("URL请求失败: %s, 错误: %v", targetURL, err)
		return fmt.Errorf("执行GET请求失败: %v", err)
	}

	f(resp)

	return nil
}

// Post 方法用于发送带请求头的 POST 请求
func Post(requestURL string, contentType string, data []byte, headers map[string]string) (*common.Response, error) {
	targetURL, err := buildURL(requestURL)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", targetURL, bytes.NewBuffer(data))
	if err != nil {
		return nil, fmt.Errorf("创建POST请求失败: %v", err)
	}

	req.Header.Set("Content-Type", contentType)
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	client, clientErr := NewHTTPClientWithProxy(config.SysConfig.GetHttpProxy(), 0, ProxyIsAvailable)
	if clientErr != nil {
		return nil, clientErr
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
				zap.S().Infof("ResponseStream complete, %s.", fileName)
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
					prom.PromRequestByteCounter(prom.RequestResponseByte, source, int64(len(b)))
				}
			}
			flusher.Flush()
		}
	}
}

func GetDomain(hfURL string) (string, error) {
	parsedURL, err := url.Parse(hfURL)
	if err != nil {
		return "", err
	}
	return parsedURL.Host, nil
}
