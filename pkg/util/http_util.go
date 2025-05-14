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
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"dingospeed/pkg/common"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	"dingospeed/pkg/prom"

	"github.com/avast/retry-go"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

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

// Head 方法用于发送带请求头的 HEAD 请求，支持主备URL重试
func Head(requestURL string, headers map[string]string, timeout time.Duration) (*common.Response, error) {
	parsedURL, err := url.Parse(requestURL)
	if err != nil {
		return nil, fmt.Errorf("解析请求URL失败: %w", err)
	}

	type urlInfo struct {
		base   string
		isMain bool
	}
	urls := []urlInfo{
		{config.SysConfig.GetHFURLBase(), true},
	}
	if config.SysConfig.DynamicProxy.Enabled {
		urls = append(urls, urlInfo{config.SysConfig.GetBpHFURLBase(), false})
	}

	var lastResp *common.Response
	var lastErr error

	for _, u := range urls {
		targetURL := u.base + parsedURL.Path
		req, err := http.NewRequest("HEAD", targetURL, nil)
		if err != nil {
			lastErr = err
			continue
		}
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		client := &http.Client{Timeout: timeout}
		resp, err := client.Do(req)
		if err != nil {
			zap.S().Warnf("%sURL请求失败: %s, 错误: %v", map[bool]string{true: "主", false: "备用"}[u.isMain], targetURL, err)
			lastErr = err
			continue
		}
		defer resp.Body.Close()

		respHeaders := make(map[string]interface{})
		for key, values := range resp.Header {
			respHeaders[key] = values
		}
		result := &common.Response{
			StatusCode: resp.StatusCode,
			Headers:    respHeaders,
		}
		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusTemporaryRedirect {
			return result, nil
		}
		zap.S().Warnf("%sURL返回无效状态码: %s, 状态码: %d", map[bool]string{true: "主", false: "备用"}[u.isMain], targetURL, resp.StatusCode)
		lastResp = result
	}
	if lastErr != nil {
		return nil, fmt.Errorf("主URL和备用URL均请求失败: %w", lastErr)
	}
	return lastResp, nil
}

// Get 方法用于发送带请求头的 GET 请求，支持主备URL重试
func Get(requestURL string, headers map[string]string, timeout time.Duration) (*common.Response, error) {
	parsedURL, err := url.Parse(requestURL)
	if err != nil {
		return nil, fmt.Errorf("解析请求URL失败: %w", err)
	}

	type urlInfo struct {
		base   string
		isMain bool
	}
	urls := []urlInfo{
		{config.SysConfig.GetHFURLBase(), true},
	}
	if config.SysConfig.DynamicProxy.Enabled {
		urls = append(urls, urlInfo{config.SysConfig.GetBpHFURLBase(), false})
	}

	var lastResp *common.Response
	var lastErr error

	for _, u := range urls {
		targetURL := u.base + parsedURL.Path
		req, err := http.NewRequest("GET", targetURL, nil)
		if err != nil {
			lastErr = err
			continue
		}
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		client := &http.Client{Timeout: timeout}
		resp, err := client.Do(req)
		if err != nil {
			zap.S().Warnf("%sURL请求失败: %s, 错误: %v", map[bool]string{true: "主", false: "备用"}[u.isMain], targetURL, err)
			lastErr = err
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		respHeaders := make(map[string]interface{})
		for key, values := range resp.Header {
			respHeaders[key] = values
		}
		result := &common.Response{
			StatusCode: resp.StatusCode,
			Headers:    respHeaders,
			Body:       body,
		}
		if resp.StatusCode == http.StatusOK {
			return result, nil
		}
		zap.S().Warnf("%sURL返回非OK状态码: %s, 状态码: %d", map[bool]string{true: "主", false: "备用"}[u.isMain], targetURL, resp.StatusCode)
		lastResp = result
	}
	if lastErr != nil {
		return nil, fmt.Errorf("主URL和备用URL均请求失败: %w", lastErr)
	}
	return lastResp, nil
}

// speedMonitoringReader 是一个包装器，用于监控读取的字节数
type speedMonitoringReader struct {
	io.ReadCloser
	onRead func(bytesRead int)
}

// Read 实现了io.Reader接口
func (r *speedMonitoringReader) Read(p []byte) (n int, err error) {
	n, err = r.ReadCloser.Read(p)
	if n > 0 && r.onRead != nil {
		r.onRead(n)
	}
	return
}

// GetStream 方法用于发送带请求头的 GET 请求，支持主备URL重试
func GetStream(requestURL string, headers map[string]string, timeout time.Duration, f func(r *http.Response)) error {
	speedThreshold := config.SysConfig.GetSpeedThreshold()
	speedCheckInterval := config.SysConfig.GetSpeedCheckInterval()
	minSlowChecks := config.SysConfig.GetMinSlowChecks()

	parsedURL, err := url.Parse(requestURL)
	if err != nil {
		return fmt.Errorf("解析请求URL失败: %w", err)
	}

	type urlInfo struct {
		base   string
		isMain bool
	}
	urls := []urlInfo{
		{config.SysConfig.GetHFURLBase(), true},
	}
	bpBase := config.SysConfig.GetBpHFURLBase()
	if config.SysConfig.DynamicProxy.Enabled && bpBase != "" && bpBase != "false" {
		urls = append(urls, urlInfo{bpBase, false})
	}

	for _, u := range urls {
		targetURL := u.base + parsedURL.Path
		client := &http.Client{Timeout: timeout}
		req, err := http.NewRequest("GET", targetURL, nil)
		if err != nil {
			zap.S().Warnf("%sURL请求构建失败: %s, 错误: %v", map[bool]string{true: "主", false: "备用"}[u.isMain], targetURL, err)
			continue
		}
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		resp, err := client.Do(req)
		if err != nil {
			zap.S().Warnf("%sURL请求失败: %s, 错误: %v", map[bool]string{true: "主", false: "备用"}[u.isMain], targetURL, err)
			continue
		}

		var (
			totalBytes   int64
			startTime    = time.Now()
			lastBytes    int64
			lastTime     = startTime
			slowCount    int32
			needSwitch   = make(chan struct{})
			readComplete = make(chan struct{})
		)

		bodyReader := resp.Body
		if u.isMain && config.SysConfig.DynamicProxy.Enabled {
			bodyReader = &speedMonitoringReader{
				ReadCloser: resp.Body,
				onRead: func(bytesRead int) {
					totalBytes += int64(bytesRead)
					now := time.Now()
					elapsed := now.Sub(lastTime).Seconds()
					if totalBytes-lastBytes > speedThreshold*2 {
						currentSpeed := float64(totalBytes-lastBytes) / elapsed
						if currentSpeed < float64(speedThreshold) {
							if atomic.AddInt32(&slowCount, 1) >= int32(minSlowChecks) {
								select {
								case <-needSwitch:
								default:
									close(needSwitch)
								}
							}
						} else {
							atomic.StoreInt32(&slowCount, 0)
						}
						lastBytes = totalBytes
						lastTime = now
					}
				},
			}
			go func() {
				time.Sleep(speedCheckInterval)
				if time.Since(startTime).Seconds() > float64(speedCheckInterval) && totalBytes < speedThreshold*int64(speedCheckInterval) {
					select {
					case <-needSwitch:
					default:
						close(needSwitch)
					}
				}
			}()
		}
		resp.Body = bodyReader
		defer resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			zap.S().Warnf("%sURL返回无效状态码: %s, 状态码: %d", map[bool]string{true: "主", false: "备用"}[u.isMain], targetURL, resp.StatusCode)
			continue
		}

		f(resp)
		close(readComplete)

		select {
		case <-needSwitch:
			totalTime := time.Since(startTime).Seconds()
			avgSpeed := float64(totalBytes) / totalTime
			zap.S().Warnf("%sURL下载速度持续低于阈值，准备切换: %.2f MB/s < %.2f MB/s", map[bool]string{true: "主", false: "备用"}[u.isMain], avgSpeed/1024/1024, float64(speedThreshold)/1024/1024)
			continue
		case <-readComplete:
			if atomic.LoadInt32(&slowCount) >= int32(minSlowChecks) {
				continue
			}
			return nil
		case <-time.After(timeout):
			return fmt.Errorf("%sURL请求超时: %s", map[bool]string{true: "主", false: "备用"}[u.isMain], targetURL)
		}
	}
	return fmt.Errorf("主URL和备用URL均请求失败或速度过慢")
}

// Post 方法用于发送带请求头的 POST 请求，支持主备URL重试
func Post(requestURL string, contentType string, data []byte, headers map[string]string) (*common.Response, error) {
	parsedURL, err := url.Parse(requestURL)
	if err != nil {
		return nil, fmt.Errorf("解析请求URL失败: %w", err)
	}

	type urlInfo struct {
		base   string
		isMain bool
	}
	urls := []urlInfo{
		{config.SysConfig.GetHFURLBase(), true},
	}
	if config.SysConfig.DynamicProxy.Enabled {
		urls = append(urls, urlInfo{config.SysConfig.GetBpHFURLBase(), false})
	}

	var lastResp *common.Response
	var lastErr error

	for _, u := range urls {
		targetURL := u.base + parsedURL.Path
		req, err := http.NewRequest("POST", targetURL, bytes.NewBuffer(data))
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Content-Type", contentType)
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			zap.S().Warnf("%sURL请求失败: %s, 错误: %v", map[bool]string{true: "主", false: "备用"}[u.isMain], targetURL, err)
			lastErr = err
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		respHeaders := make(map[string]interface{})
		for key, values := range resp.Header {
			respHeaders[key] = values
		}
		result := &common.Response{
			StatusCode: resp.StatusCode,
			Headers:    respHeaders,
			Body:       body,
		}
		if resp.StatusCode == http.StatusOK {
			return result, nil
		}
		zap.S().Warnf("%sURL返回非OK状态码: %s, 状态码: %d", map[bool]string{true: "主", false: "备用"}[u.isMain], targetURL, resp.StatusCode)
		lastResp = result
	}
	if lastErr != nil {
		return nil, fmt.Errorf("主URL和备用URL均请求失败: %w", lastErr)
	}
	return lastResp, nil
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
