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

type urlInfo struct {
	base   string
	isMain bool
}

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

func buildURLList(requestURL string) ([]urlInfo, *url.URL, error) {
	parsedURL, err := url.Parse(requestURL)
	if err != nil {
		return nil, nil, fmt.Errorf("解析请求URL失败: %w", err)
	}
	urls := []urlInfo{
		{config.SysConfig.GetHFURLBase(), true},
	}
	if config.SysConfig.DynamicProxy.Enabled {
		urls = append(urls, urlInfo{config.SysConfig.GetBpHFURLBase(), false})
	}
	return urls, parsedURL, nil
}

// Head 方法用于发送带请求头的 HEAD 请求，支持主备URL重试
func Head(requestURL string, headers map[string]string, timeout time.Duration) (*common.Response, error) {
	urls, parsedURL, err := buildURLList(requestURL)
	if err != nil {
		return nil, err
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
		var client *http.Client
		// 主URL和备用URL都需要配置代理，但需判断代理是否可用
		proxyAddr := config.SysConfig.GetHttpProxy()
		if proxyAddr != "" {
			proxyURL, err := url.Parse(proxyAddr)
			if err == nil {
				client = &http.Client{
					Timeout: timeout,
					Transport: &http.Transport{
						Proxy: http.ProxyURL(proxyURL),
					},
				}
			} else {
				zap.S().Warnf("代理地址无效，未设置代理: %s, 错误: %v", proxyAddr, err)
				client = &http.Client{Timeout: timeout}
			}
		}
		resp, err := client.Do(req)
		if err != nil {
			zap.S().Warnf("%sURL请求失败: %s, 错误: %v", map[bool]string{true: "主", false: "备用"}[u.isMain], targetURL, err)
			lastErr = err
			continue
		}

		func() {
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
				lastResp = result
				lastErr = nil
				return
			}
			zap.S().Warnf("%sURL返回无效状态码: %s, 状态码: %d", map[bool]string{true: "主", false: "备用"}[u.isMain], targetURL, resp.StatusCode)
			lastResp = result
		}()
		if lastErr == nil {
			return lastResp, nil
		}
	}
	if lastErr != nil {
		return nil, fmt.Errorf("主URL和备用URL均请求失败: %w", lastErr)
	}
	return lastResp, nil
}

// Get 方法用于发送带请求头的 GET 请求，支持主备URL重试
func Get(requestURL string, headers map[string]string, timeout time.Duration) (*common.Response, error) {
	urls, parsedURL, err := buildURLList(requestURL)
	if err != nil {
		return nil, err
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

		var client *http.Client
		// 主URL和备用URL都需要配置代理，但需判断代理是否可用
		proxyAddr := config.SysConfig.GetHttpProxy()
		if proxyAddr != "" {
			proxyURL, err := url.Parse(proxyAddr)
			if err == nil {
				client = &http.Client{
					Timeout: timeout,
					Transport: &http.Transport{
						Proxy: http.ProxyURL(proxyURL),
					},
				}
			} else {
				zap.S().Warnf("代理地址无效，未设置代理: %s, 错误: %v", proxyAddr, err)
				client = &http.Client{Timeout: timeout}
			}
		}
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

func GetStream(requestURL string, headers map[string]string, timeout time.Duration, startPos, endPos int64, fileName string, f func(r *http.Response)) error {
	var (
		startTime    = time.Now()
		lastBytes    int64
		lastTime     = startTime
		slowCount    int32
		totalBytes   int64
		currentSpeed float64
	)

	speedThreshold := config.SysConfig.GetSpeedThreshold()
	minSlowChecks := config.SysConfig.GetMinSlowChecks()
	maxSwitchCount := config.SysConfig.GetMaxSwitchCount()
	bytesDeltaThreshold := config.SysConfig.GetBytesDeltaThreshold()

	parsedURL, err := url.Parse(requestURL)
	if err != nil {
		return fmt.Errorf("解析请求URL失败: %w", err)
	}

	urls := []urlInfo{
		{config.SysConfig.GetHFURLBase(), true},
	}
	bpBase := config.SysConfig.GetBpHFURLBase()
	if config.SysConfig.DynamicProxy.Enabled && bpBase != "" && bpBase != "false" {
		urls = append(urls, urlInfo{bpBase, false})
	} else if !config.SysConfig.DynamicProxy.Enabled {
		maxSwitchCount = 1
	}

	switchCount := 0
	urlIndex := 0

	for {
		u := urls[urlIndex]
		targetURL := u.base + parsedURL.Path
		var client *http.Client
		// 主URL和备用URL都需要配置代理，但需判断代理是否可用
		proxyAddr := config.SysConfig.GetHttpProxy()
		if proxyAddr != "" {
			proxyURL, err := url.Parse(proxyAddr)
			if err == nil {
				client = &http.Client{
					Timeout: timeout,
					Transport: &http.Transport{
						Proxy: http.ProxyURL(proxyURL),
					},
				}
			} else {
				zap.S().Warnf("代理地址无效，未设置代理: %s, 错误: %v", proxyAddr, err)
				client = &http.Client{Timeout: timeout}
			}
		}

		req, err := http.NewRequest("GET", targetURL, nil)
		if err != nil {
			zap.S().Warnf("%sURL请求构建失败: %s, 错误: %v", map[bool]string{true: "主", false: "备用"}[u.isMain], targetURL, err)
			switchCount++
			if switchCount >= len(urls) {
				return fmt.Errorf("所有URL请求构建均失败")
			}
			urlIndex = (urlIndex + 1) % len(urls)
			continue
		}

		headers["range"] = fmt.Sprintf("bytes=%d-%d", startPos+totalBytes, endPos-1)
		for key, value := range headers {
			req.Header.Set(key, value)
		}

		// 每次请求前创建新的切换通道
		needSwitch := make(chan struct{})
		resp, err := client.Do(req)
		if err != nil {
			zap.S().Warnf("%sURL请求失败: %s, 错误: %v", map[bool]string{true: "主", false: "备用"}[u.isMain], targetURL, err)
			switchCount++
			if switchCount >= len(urls) {
				return fmt.Errorf("所有URL请求均失败")
			}
			urlIndex = (urlIndex + 1) % len(urls)
			continue
		}

		bodyReader := resp.Body
		if switchCount > 0 {
			atomic.StoreInt32(&slowCount, 0)
		}

		if config.SysConfig.DynamicProxy.Enabled && switchCount < maxSwitchCount {
			bodyReader = &speedMonitoringReader{
				ReadCloser: resp.Body,
				onRead: func(bytesRead int) {
					totalBytes += int64(bytesRead)
					now := time.Now()
					elapsed := now.Sub(lastTime).Seconds()
					if totalBytes-lastBytes > bytesDeltaThreshold {
						currentSpeed = float64(totalBytes-lastBytes) / elapsed
						if currentSpeed < float64(speedThreshold) {
							newSlowCount := atomic.AddInt32(&slowCount, 1)
							zap.S().Debugf("慢速检测计数: %d/%d，文件: %s，速度: %.2f MB/s", newSlowCount, minSlowChecks, fileName, currentSpeed/1024/1024)
							if newSlowCount >= int32(minSlowChecks) && switchCount < maxSwitchCount {
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
		}
		resp.Body = bodyReader

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			resp.Body.Close()
			zap.S().Warnf("%sURL返回无效状态码: %s, 状态码: %d", map[bool]string{true: "主", false: "备用"}[u.isMain], targetURL, resp.StatusCode)
			switchCount++
			if switchCount >= len(urls) {
				return fmt.Errorf("所有URL均返回无效状态码")
			}
			urlIndex = (urlIndex + 1) % len(urls)
			continue
		}

		// 启动协程监听切换信号
		switchListener := make(chan struct{})
		go func() {
			select {
			case <-needSwitch:
				resp.Body.Close()
				close(switchListener)
			}
		}()

		f(resp)
		resp.Body.Close()

		if totalBytes == endPos-startPos {
			return nil
		}

		// 等待切换信号或完成
		select {
		case <-switchListener:
			zap.S().Warnf("%sURL下载速度持续低于阈值，准备切换: %.2f MB/s < %.2f MB/s", map[bool]string{true: "主", false: "备用"}[u.isMain], currentSpeed/1024/1024, float64(speedThreshold)/1024/1024)
			switchCount++
			if switchCount >= maxSwitchCount {
				zap.S().Warnf("已达最大切换次数，继续使用当前URL: %s", targetURL)
			} else {
				urlIndex = (urlIndex + 1) % len(urls)
				continue
			}
		default:
			// 继续正常执行
		}
	}
}

// Post 方法用于发送带请求头的 POST 请求，支持主备URL重试
func Post(requestURL string, contentType string, data []byte, headers map[string]string) (*common.Response, error) {
	urls, parsedURL, err := buildURLList(requestURL)
	if err != nil {
		return nil, err
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

		var client *http.Client
		// 主URL和备用URL都需要配置代理，但需判断代理是否可用
		proxyAddr := config.SysConfig.GetHttpProxy()
		if proxyAddr != "" {
			proxyURL, err := url.Parse(proxyAddr)
			if err == nil {
				client = &http.Client{
					Transport: &http.Transport{
						Proxy: http.ProxyURL(proxyURL),
					},
				}
			} else {
				zap.S().Warnf("代理地址无效，未设置代理: %s, 错误: %v", proxyAddr, err)
				client = &http.Client{}
			}
		}
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
