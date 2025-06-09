package service

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"dingospeed/pkg/config"
	"dingospeed/pkg/util"

	"github.com/shirou/gopsutil/mem"
	"go.uber.org/zap"
)

var once sync.Once

type SysService struct {
}

func NewSysService() *SysService {
	sysSvc := &SysService{}
	once.Do(
		func() {
			if config.SysConfig.Cache.Enabled {
				go sysSvc.MemoryUsed()
			}

			if config.SysConfig.DiskClean.Enabled {
				go sysSvc.cycleCheckDiskUsage()
			}

			testProxyConnectivity()
			if config.SysConfig.DynamicProxy.Enabled {
				go sysSvc.cycleTestProxyConnectivity()
			}
		})
	return sysSvc
}

func (s SysService) MemoryUsed() {
	ticker := time.NewTicker(config.SysConfig.GetCollectTimePeriod())
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			memoryInfo, err := mem.VirtualMemory()
			if err != nil {
				fmt.Printf("获取内存信息时出错: %v\n", err)
				continue
			}
			config.SystemInfo.SetMemoryUsed(time.Now().Unix(), memoryInfo.UsedPercent)
		}
	}
}

func (s SysService) cycleCheckDiskUsage() {
	ticker := time.NewTicker(config.SysConfig.GetDiskCollectTimePeriod())
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			checkDiskUsage()
		}
	}
}

// 检查磁盘使用情况
func checkDiskUsage() {
	if !config.SysConfig.Online() {
		return
	}
	if !config.SysConfig.DiskClean.Enabled {
		return
	}

	currentSize, err := util.GetFolderSize(config.SysConfig.Repos())
	if err != nil {
		zap.S().Errorf("Error getting folder size: %v", err)
		return
	}

	limitSize := config.SysConfig.DiskClean.CacheSizeLimit
	limitSizeH := util.ConvertBytesToHumanReadable(limitSize)
	currentSizeH := util.ConvertBytesToHumanReadable(currentSize)

	if currentSize < limitSize {
		return
	}

	zap.S().Infof("Cache size exceeded! Limit: %s, Current: %s.\n", limitSizeH, currentSizeH)
	zap.S().Infof("Cleaning...")

	filesPath := filepath.Join(config.SysConfig.Repos(), "files")

	var allFiles []util.FileWithPath
	switch config.SysConfig.CacheCleanStrategy() {
	case "LRU":
		allFiles, err = util.SortFilesByAccessTime(filesPath)
		if err != nil {
			zap.S().Errorf("Error sorting files by access time in %s: %v\n", filesPath, err)
			return
		}
	case "FIFO":
		allFiles, err = util.SortFilesByModifyTime(filesPath)
		if err != nil {
			zap.S().Errorf("Error sorting files by modify time in %s: %v\n", filesPath, err)
			return
		}
	case "LARGE_FIRST":
		allFiles, err = util.SortFilesBySize(filesPath)
		if err != nil {
			zap.S().Errorf("Error sorting files by size in %s: %v\n", filesPath, err)
			return
		}
	default:
		zap.S().Errorf("Unknown cache clean strategy: %s\n", config.SysConfig.CacheCleanStrategy())
		return
	}

	for _, file := range allFiles {
		if currentSize < limitSize {
			break
		}
		filePath := file.Path
		fileSize := file.Info.Size()
		err := os.Remove(filePath)
		if err != nil {
			zap.S().Errorf("Error removing file %s: %v\n", filePath, err)
			continue
		}
		currentSize -= fileSize
		zap.S().Infof("Remove file: %s. File Size: %s\n", filePath, util.ConvertBytesToHumanReadable(fileSize))
	}

	currentSize, err = util.GetFolderSize(config.SysConfig.Repos())
	if err != nil {
		zap.S().Errorf("Error getting folder size after cleaning: %v\n", err)
		return
	}
	currentSizeH = util.ConvertBytesToHumanReadable(currentSize)
	zap.S().Infof("Cleaning finished. Limit: %s, Current: %s.\n", limitSizeH, currentSizeH)
}

func (s SysService) cycleTestProxyConnectivity() {
	ticker := time.NewTicker(config.SysConfig.GetDynamicProxyTimePeriod())
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			testProxyConnectivity()
		}
	}
}

// 测试代理连通性
func testProxyConnectivity() {
	proxyURL, err := url.Parse(config.SysConfig.GetHttpProxy())
	if err != nil {
		util.ProxyIsAvailable = false
		zap.S().Warnf("代理测试请求创建失败: %v", err)
		return
	}

	// 创建一个短期超时的HTTP客户端用于测试
	testClient := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
			DialContext: (&net.Dialer{
				Timeout: 3 * time.Second,
			}).DialContext,
		},
	}

	// 尝试访问一个轻量级的公共URL
	req, err := http.NewRequest("GET", "https://www.google.com", nil)
	if err != nil {
		util.ProxyIsAvailable = false
		zap.S().Warnf("代理测试请求创建失败: %v", err)
		return
	}

	// 设置合理的请求头，避免被反爬机制拦截
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; proxy-test/1.0)")

	// 执行请求
	resp, err := testClient.Do(req)
	if err != nil {
		util.ProxyIsAvailable = false
		zap.S().Warnf("代理测试失败: %v", err)
		return
	}
	defer resp.Body.Close()

	// 检查HTTP状态码
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		zap.S().Infof("代理测试成功: %s", proxyURL.String())
		return
	}

	util.ProxyIsAvailable = false
	zap.S().Warnf("代理测试返回非成功状态码: %d", resp.StatusCode)
	return
}
