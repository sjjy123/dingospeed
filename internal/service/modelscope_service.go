package service

import (
	"context"
	"dingospeed/pkg/config"
	"dingospeed/pkg/util"
	"fmt"
	"github.com/gofrs/flock"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type ModelscopeService struct {
	downloadingMap sync.Map
	mapMu          sync.Mutex
}

func NewModelscopeService() *ModelscopeService {
	return &ModelscopeService{
		downloadingMap: sync.Map{},
		mapMu:          sync.Mutex{},
	}
}

// CalculateBlockInfo 计算块信息：块编号、块起始偏移、块结束偏移
func (m *ModelscopeService) CalculateBlockInfo(offset int64, chunkSize int64) (blockNum int64, blockStart int64, blockEnd int64) {
	blockNum = offset / chunkSize
	blockStart = blockNum * chunkSize
	blockEnd = blockStart + chunkSize - 1
	return
}

// IsBlockComplete 检查指定块是否完整
func (m *ModelscopeService) IsBlockComplete(cachePath string, blockNum int64, chunkSize int64) (bool, error) {
	fileInfo, err := os.Stat(cachePath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat cache file failed: %w", err)
	}

	blockStart := blockNum * chunkSize
	blockEnd := blockStart + chunkSize - 1
	fileSize := fileInfo.Size()

	if fileSize <= blockStart {
		return false, nil // 块为空
	}
	if fileSize > blockEnd {
		return true, nil // 块完整
	}
	return false, nil
}

// getCacheLock 获取缓存文件对应的锁文件
func (m *ModelscopeService) getCacheLock(cachePath string) *flock.Flock {
	lockPath := cachePath + ".lock"
	return flock.New(lockPath)
}

func (m *ModelscopeService) openCacheFile(cachePath string, c echo.Context) (*os.File, *flock.Flock, error) {
	lockPath := cachePath + ".lock"
	fileLock := flock.New(lockPath)

	ctx, cancel := context.WithTimeout(c.Request().Context(), 3*time.Second)
	defer cancel()
	locked, lockErr := fileLock.TryLockContext(ctx, time.Duration(0))
	if lockErr != nil {
		zap.S().Errorf("获取缓存文件锁失败: %s, err: %v", lockPath, lockErr)
		return nil, nil, c.JSON(http.StatusInternalServerError, map[string]string{
			"code":  "500",
			"error": "get cache file lock failed",
			"msg":   lockErr.Error(),
		})
	}
	if !locked {
		zap.S().Warnf("缓存文件锁超时，无法独占写入: %s", cachePath)
		return nil, nil, c.JSON(http.StatusTooManyRequests, map[string]string{
			"code":  "429",
			"error": "cache file is being written by another request",
			"msg":   "please try again later",
		})
	}

	var cacheFile *os.File
	var err error
	if _, statErr := os.Stat(cachePath); os.IsNotExist(statErr) {
		cacheFile, err = os.OpenFile(cachePath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0664)
	} else {
		cacheFile, err = os.OpenFile(cachePath, os.O_RDWR, 0664)
	}

	if err != nil {
		zap.S().Errorf("打开缓存文件失败: %s, err: %v", cachePath, err)
		_ = fileLock.Unlock()
		return nil, nil, c.JSON(http.StatusInternalServerError, map[string]string{
			"code":  "500",
			"error": "open cache file failed",
			"msg":   err.Error(),
		})
	}

	return cacheFile, fileLock, nil
}

func (m *ModelscopeService) copyResponseToCacheAndClient(c echo.Context, resp *http.Response, cacheFile *os.File, cachePath string, totalFileSize int64) error {
	chunkSize := config.SysConfig.Modelscope.ChunkSize
	if chunkSize <= 0 {
		chunkSize = 1024 * 1024 * 8
	}

	currentOffset := int64(0)
	contentRange := resp.Header.Get("Content-Range")
	if contentRange != "" {
		parts := strings.Split(contentRange, " ")
		if len(parts) >= 2 {
			rangeParts := strings.Split(parts[1], "-")
			if len(rangeParts) >= 1 {
				parsedOffset, err := strconv.ParseInt(rangeParts[0], 10, 64)
				if err == nil {
					currentOffset = parsedOffset
				}
			}
		}
	}

	buf := make([]byte, chunkSize)
	written := int64(0)

	for {
		if c.Request().Context().Err() != nil {
			zap.S().Warnf("客户端断开连接，停止续传: %s", cachePath)
			if cacheFile != nil {
				_ = cacheFile.Sync()
				closeErr := cacheFile.Close()
				if closeErr != nil {
					zap.S().Errorf("关闭缓存文件失败: %s, err: %v", cachePath, closeErr)
				}
			}
			return nil
		}

		currentWriteOffset := currentOffset + written
		blockNum, _, blockEnd := m.CalculateBlockInfo(currentWriteOffset, chunkSize)

		isComplete, err := m.IsBlockComplete(cachePath, blockNum, chunkSize)
		if err != nil {
			zap.S().Errorf("检查块%d完整性失败: %v", blockNum, err)
			return c.JSON(http.StatusInternalServerError, map[string]string{
				"code":  "500",
				"error": "check block complete failed",
				"msg":   err.Error(),
			})
		}

		if isComplete {
			skipBytes := blockEnd - currentWriteOffset + 1
			written += skipBytes
			continue
		}

		readSize := blockEnd - currentWriteOffset + 1
		if readSize > int64(len(buf)) {
			readSize = int64(len(buf))
		}

		n, err := resp.Body.Read(buf[:readSize])
		if n > 0 {
			if _, seekErr := cacheFile.Seek(currentWriteOffset, io.SeekStart); seekErr != nil {
				zap.S().Errorf("定位缓存文件到偏移%d失败: %v", currentWriteOffset, seekErr)
				return c.JSON(http.StatusInternalServerError, map[string]string{
					"code":  "500",
					"error": "seek cache file failed",
					"msg":   seekErr.Error(),
				})
			}

			if _, writeErr := cacheFile.Write(buf[:n]); writeErr != nil {
				zap.S().Errorf("写入缓存块%d失败: %s, err: %v", blockNum, cachePath, writeErr)
				return c.JSON(http.StatusInternalServerError, map[string]string{
					"code":  "500",
					"error": "write cache block failed",
					"msg":   writeErr.Error(),
				})
			}
			if syncErr := cacheFile.Sync(); syncErr != nil {
				zap.S().Warnf("缓存块%d刷盘失败: %s, err: %v", blockNum, cachePath, syncErr)
			}

			if _, writeErr := c.Response().Write(buf[:n]); writeErr != nil {
				if strings.Contains(writeErr.Error(), "http2: stream closed") ||
					strings.Contains(writeErr.Error(), "broken pipe") ||
					strings.Contains(writeErr.Error(), "connection reset by peer") {
					zap.S().Warnf("客户端断开连接，停止返回续传数据: %s, err: %v", cachePath, writeErr)
					return nil
				}
				zap.S().Errorf("返回续传数据失败: %s, err: %v", cachePath, writeErr)
				return c.JSON(http.StatusInternalServerError, map[string]string{
					"code":  "500",
					"error": "write response failed",
					"msg":   writeErr.Error(),
				})
			}

			written += int64(n)
			if f, ok := c.Response().Writer.(http.Flusher); ok {
				f.Flush()
			}

			if written%(100*1024*1024) == 0 {
				zap.S().Infof("续传进度: %dMB, 文件: %s", written/(1024*1024), cachePath)
			}
		}

		if err == io.EOF {
			if syncErr := cacheFile.Sync(); syncErr != nil {
				zap.S().Warnf("缓存文件最终刷盘失败: %s, err: %v", cachePath, syncErr)
			}
			zap.S().Infof("续传完成: %s, 共下载%d字节，完整文件%d字节", cachePath, written, totalFileSize)
			break
		}
		if err != nil {
			zap.S().Errorf("续传中断: %s, err: %v", cachePath, err)
			return c.JSON(http.StatusBadGateway, map[string]string{
				"code":  "502",
				"error": "download interrupted",
				"msg":   err.Error(),
			})
		}
	}

	return nil
}

func (m *ModelscopeService) ForwardModelInfo(c echo.Context, owner, repo string, repoType string) error {
	apiPrefix := util.GetAPIPathPrefix(repoType)
	officialURL := fmt.Sprintf("%s/api/v1/%s/%s/%s?%s",
		config.SysConfig.Modelscope.OfficialBaseURL,
		apiPrefix,
		url.PathEscape(owner),
		url.PathEscape(repo),
		c.Request().URL.RawQuery)
	zap.S().Infof("转发%s信息请求到官方: %s", apiPrefix, officialURL)
	return m.forwardRequest(c, officialURL)
}

func (m *ModelscopeService) ForwardRevisions(c echo.Context, owner, repo string, repoType string) error {
	apiPrefix := util.GetAPIPathPrefix(repoType)
	officialURL := fmt.Sprintf("%s/api/v1/%s/%s/%s/revisions?%s",
		config.SysConfig.Modelscope.OfficialBaseURL,
		apiPrefix,
		url.PathEscape(owner),
		url.PathEscape(repo),
		c.Request().URL.RawQuery)
	zap.S().Infof("转发%s版本请求到官方: %s", apiPrefix, officialURL)
	return m.forwardRequest(c, officialURL)
}

func (m *ModelscopeService) ForwardFileList(c echo.Context, owner, repo string, repoType string) error {
	apiPrefix := util.GetAPIPathPrefix(repoType)
	officialURL := fmt.Sprintf("%s/api/v1/%s/%s/%s/repo/files?%s",
		config.SysConfig.Modelscope.OfficialBaseURL,
		apiPrefix,
		url.PathEscape(owner),
		url.PathEscape(repo),
		c.Request().URL.RawQuery)
	zap.S().Infof("转发%s文件列表请求到官方: %s", apiPrefix, officialURL)
	return m.forwardRequest(c, officialURL)
}

func (m *ModelscopeService) ForwardRepoTree(c echo.Context, owner, repo string, repoType string) error {
	apiPrefix := util.GetAPIPathPrefix(repoType)
	officialURL := fmt.Sprintf("%s/api/v1/%s/%s/%s/repo/tree?%s",
		config.SysConfig.Modelscope.OfficialBaseURL,
		apiPrefix,
		url.PathEscape(owner),
		url.PathEscape(repo),
		c.Request().URL.RawQuery)
	zap.S().Infof("转发%s文件树请求到官方: %s", apiPrefix, officialURL)
	return m.forwardRequest(c, officialURL)
}

func (m *ModelscopeService) ForwardRepoTreeByDatasetId(c echo.Context, datasetId string) error {
	officialURL := fmt.Sprintf("%s/api/v1/datasets/%s/repo/tree?%s",
		config.SysConfig.Modelscope.OfficialBaseURL,
		url.PathEscape(datasetId),
		c.Request().URL.RawQuery)
	zap.S().Infof("转发文件树请求到官方: %s", officialURL)
	return m.forwardRequest(c, officialURL)
}

// forwardRequest 通用请求转发逻辑
func (m *ModelscopeService) forwardRequest(c echo.Context, officialURL string) error {
	req, err := http.NewRequest(http.MethodGet, officialURL, nil)
	if err != nil {
		zap.S().Errorf("构建请求失败: %v", err)
		return err
	}

	util.AddCLIHeaders(req.Header, c.Request().Header.Get("User-Agent"))

	for k, v := range c.Request().Header {
		req.Header[k] = v
	}

	resp, err := util.DoRequestWithRetry(req)
	if err != nil {
		zap.S().Errorf("转发请求失败: %v", err)
		return err
	}
	defer resp.Body.Close()

	for k, v := range resp.Header {
		c.Response().Header()[k] = v
	}
	c.Response().WriteHeader(resp.StatusCode)

	_, err = io.Copy(c.Response(), resp.Body)
	if err != nil {
		zap.S().Errorf("复制响应体失败: %v", err)
		return err
	}
	return nil
}

// HandleFileDownload 处理ModelScope文件下载请求
func (m *ModelscopeService) HandleFileDownload(c echo.Context, owner, repo, repoType string) error {
	repoId := fmt.Sprintf("%s/%s", owner, repo)
	revision := c.Request().URL.Query().Get("Revision")
	filePath := c.Request().URL.Query().Get("FilePath")

	if revision == "" {
		revision = "master"
	}
	if filePath == "" {
		zap.S().Error("请求参数缺失: FilePath为空")
		return c.JSON(http.StatusBadRequest, map[string]string{
			"code":  "400",
			"error": "missing FilePath parameter",
		})
	}

	if err := util.EnsureDir(filepath.Join(config.SysConfig.GetModelCacheRoot(), "dummy")); err != nil {
		zap.S().Errorf("初始化模型缓存根目录失败: %v", err)
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"code":  "500",
			"error": "init model cache root dir failed",
			"msg":   err.Error(),
		})
	}

	cachePath, cacheExists := util.GetCachePath(repoType, repoId, revision, filePath)
	if cachePath == "" {
		zap.S().Errorf("生成缓存路径失败: 无效的repoId格式 %s", repoId)
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"code":  "500",
			"error": "get cache path failed",
			"msg":   "invalid repoId format, require org/repo",
		})
	}
	zap.S().Infof("生成缓存路径: %s (缓存文件是否存在: %t)", cachePath, cacheExists)

	if err := util.EnsureDir(filepath.Dir(cachePath)); err != nil {
		zap.S().Errorf("初始化缓存文件上级目录失败: %s, err: %v", filepath.Dir(cachePath), err)
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"code":  "500",
			"error": "init cache file parent dir failed",
			"msg":   err.Error(),
		})
	}

	cachedSize := util.GetFileSize(cachePath)
	zap.S().Infof("缓存文件状态: %s (已下载: %d字节)", cachePath, cachedSize)
	if cacheExists && cachedSize == 0 {
		zap.S().Warnf("缓存文件存在但大小为0，视为无效缓存: %s", cachePath)
		if err := os.Remove(cachePath); err != nil {
			zap.S().Errorf("删除空缓存文件失败: %s, err: %v", cachePath, err)
		}
		cachedSize = 0
		cacheExists = false
	}

	clientStart, clientEnd, err := util.ParseRangeHeader(c.Request())
	if err != nil {
		zap.S().Errorf("解析Range失败: %v", err)
		return c.JSON(http.StatusBadRequest, map[string]string{
			"code":  "400",
			"error": "parse Range header failed",
			"msg":   err.Error(),
		})
	}

	actualStart := clientStart
	if cachedSize > 0 && actualStart < cachedSize {
		actualStart = cachedSize
	}
	zap.S().Infof("续传起始位置: 客户端请求=%d, 缓存末尾=%d, 实际起始=%d", clientStart, cachedSize, actualStart)

	headerWritten := false
	c.Response().Header().Set("Transfer-Encoding", "chunked")
	c.Response().Header().Set("Content-Type", "application/octet-stream")
	c.Response().Header().Set("Access-Control-Expose-Headers", "Content-Range, Content-Type")

	var cacheWritten int64 = 0
	var needRemoteDownload bool
	if cachedSize > 0 && clientStart < cachedSize {
		cacheWritten, headerWritten, needRemoteDownload, err = m.writeCacheData(c, cachePath, clientStart, clientEnd, cachedSize, headerWritten)
		if err != nil {
			zap.S().Errorf("读取缓存数据失败: %v", err)
			return c.JSON(http.StatusInternalServerError, map[string]string{
				"code":  "500",
				"error": "write cache data failed",
				"msg":   err.Error(),
			})
		}

		if clientEnd != -1 && (clientStart+cacheWritten-1) >= clientEnd {
			zap.S().Infof("缓存数据已满足客户端Range请求，无需续传")
			return nil
		}

		if needRemoteDownload {
			m.mapMu.Lock()
			defer m.mapMu.Unlock()

			isDownloading := false
			loaded := m.downloadingMap.CompareAndSwap(cachePath, false, true)
			if !loaded {
				val, exists := m.downloadingMap.Load(cachePath)
				if exists && val.(bool) {
					isDownloading = true
				} else {
					m.downloadingMap.Store(cachePath, true)
				}
			}

			if !isDownloading {
				zap.S().Infof("触发远程下载: 连续10次空轮询，当前偏移=%d, 文件=%s (标记为已发起远程下载)", clientStart+cacheWritten, cachePath)
				actualStart = clientStart + cacheWritten
			} else {
				zap.S().Infof("文件%s已发起远程下载，不重复触发，继续轮询缓存", cachePath)
				cacheWritten, headerWritten, needRemoteDownload, err = m.writeCacheData(c, cachePath, clientStart+cacheWritten, clientEnd, cachedSize, headerWritten)
				if err != nil {
					zap.S().Errorf("再次轮询缓存数据失败: %v", err)
					return c.JSON(http.StatusInternalServerError, map[string]string{
						"code":  "500",
						"error": "retry write cache data failed",
						"msg":   err.Error(),
					})
				}
				actualStart = clientStart + cacheWritten
				if needRemoteDownload {
					zap.S().Warnf("文件%s已标记为下载中，但仍触发远程下载请求，终止流程", cachePath)
					return nil
				}
			}
		}
	}

	if err := m.downloadAndWriteRemaining(c, owner, repo, actualStart, clientEnd, cachePath, headerWritten, repoType); err != nil {
		zap.S().Errorf("远程下载并写入剩余数据失败: %v", err)
		m.downloadingMap.Delete(cachePath)
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"code":  "500",
			"error": "download and write remaining data failed",
			"msg":   err.Error(),
		})
	}

	// 下载完成后清除标识
	m.downloadingMap.Delete(cachePath)
	zap.S().Infof("文件%s远程下载完成，清除下载标识", cachePath)

	return nil
}

func (m *ModelscopeService) writeCacheData(c echo.Context, cachePath string, clientStart, clientEnd, cachedSize int64, headerWritten bool) (int64, bool, bool, error) {
	pollInterval := 1 * time.Second // 每秒轮询一次缓存
	maxPollCount := 30              // 最大连续空轮询次数

	totalWritten := int64(0)
	currentCacheOffset := clientStart // 当前读取到的缓存偏移量
	emptyPollCount := 0               // 连续空轮询计数器

	for {
		// 客户端断开连接则直接返回
		if c.Request().Context().Err() != nil {
			zap.S().Warnf("客户端断开连接，停止返回缓存数据: %s", cachePath)
			return totalWritten, headerWritten, false, nil
		}

		cacheFile, err := os.Open(cachePath)
		if err != nil {
			zap.S().Errorf("打开缓存文件失败: %s, err: %v", cachePath, err)
			return totalWritten, headerWritten, false, fmt.Errorf("open cache file failed: %w", err)
		}

		fileInfo, err := cacheFile.Stat()
		if err != nil {
			_ = cacheFile.Close()
			zap.S().Errorf("获取缓存文件信息失败: %s, err: %v", cachePath, err)
			return totalWritten, headerWritten, false, fmt.Errorf("stat cache file failed: %w", err)
		}
		latestCacheSize := fileInfo.Size()

		cacheEnd := latestCacheSize - 1
		if clientEnd != -1 && clientEnd < cacheEnd {
			cacheEnd = clientEnd
		}

		if currentCacheOffset > cacheEnd {
			emptyPollCount++
			_ = cacheFile.Close()

			isDownloading := false
			if downloadingVal, exists := m.downloadingMap.Load(cachePath); exists {
				isDownloading = downloadingVal.(bool)
			}

			zap.S().Debugf("缓存暂未更新，第%d次空轮询（最大%d次）: 当前偏移=%d, 缓存末尾=%d, 文件=%s, 有客户端下载中: %t",
				emptyPollCount, maxPollCount, currentCacheOffset, cacheEnd, cachePath, isDownloading)

			if emptyPollCount >= maxPollCount && !isDownloading {
				zap.S().Warnf("连续%d次空轮询且无客户端下载中，触发当前客户端发起远程下载: %s", maxPollCount, cachePath)
				return totalWritten, headerWritten, true, nil
			}

			zap.S().Debugf("继续轮询缓存: 文件=%s, 原因: %s", cachePath,
				func() string {
					if isDownloading {
						return "有客户端正在远程下载"
					}
					return fmt.Sprintf("空轮询次数未到上限（当前%d次，最大%d次）", emptyPollCount, maxPollCount)
				}())
			time.Sleep(pollInterval)
			continue
		}

		emptyPollCount = 0
		if !headerWritten {
			contentRange := fmt.Sprintf("bytes %d-%d/%d", clientStart, cacheEnd, latestCacheSize)
			c.Response().Header().Set("Content-Range", contentRange)
			c.Response().WriteHeader(http.StatusPartialContent)
			headerWritten = true
			zap.S().Infof("设置缓存响应头: Content-Range=%s", contentRange)
		}

		if _, err := cacheFile.Seek(currentCacheOffset, io.SeekStart); err != nil {
			_ = cacheFile.Close()
			zap.S().Errorf("定位缓存文件失败: %s, err: %v", cachePath, err)
			return totalWritten, headerWritten, false, fmt.Errorf("seek cache file failed: %w", err)
		}

		buf := make([]byte, config.SysConfig.Modelscope.ChunkSize)
		readableSize := cacheEnd - currentCacheOffset + 1
		writtenInRound := int64(0)

		for writtenInRound < readableSize {
			if c.Request().Context().Err() != nil {
				_ = cacheFile.Close()
				zap.S().Warnf("客户端断开连接，停止返回缓存数据: %s", cachePath)
				return totalWritten, headerWritten, false, nil
			}

			readSize := readableSize - writtenInRound
			if readSize > int64(len(buf)) {
				readSize = int64(len(buf))
			}

			n, err := cacheFile.Read(buf[:readSize])
			if n > 0 {
				if _, writeErr := c.Response().Write(buf[:n]); writeErr != nil {
					_ = cacheFile.Close()
					zap.S().Errorf("返回缓存数据失败: %s, err: %v", cachePath, writeErr)
					return totalWritten, headerWritten, false, fmt.Errorf("write cache data to response failed: %w", writeErr)
				}

				writtenInRound += int64(n)
				totalWritten += int64(n)
				currentCacheOffset += int64(n)

				if f, ok := c.Response().Writer.(http.Flusher); ok {
					f.Flush()
				}
			}

			if err == io.EOF {
				break
			}
			if err != nil {
				_ = cacheFile.Close()
				zap.S().Errorf("读取缓存数据失败: %s, err: %v", cachePath, err)
				return totalWritten, headerWritten, false, fmt.Errorf("read cache file failed: %w", err)
			}
		}
		_ = cacheFile.Close()
		zap.S().Debugf("本轮读取缓存完成: 文件=%s, 读取字节=%d, 累计读取=%d, 当前偏移=%d",
			cachePath, writtenInRound, totalWritten, currentCacheOffset)

		if clientEnd == -1 || currentCacheOffset > clientEnd {
			zap.S().Infof("缓存数据已满足客户端Range请求: 文件=%s, 累计读取=%d字节", cachePath, totalWritten)
			return totalWritten, headerWritten, false, nil
		}

		time.Sleep(pollInterval)
	}
}

// downloadAndWriteRemaining 下载剩余部分并写入响应+缓存
func (m *ModelscopeService) downloadAndWriteRemaining(c echo.Context, owner, repo string, actualStart, clientEnd int64, cachePath string, headerWritten bool, repoType string) error {
	req, err := m.buildDownloadRequest(c, owner, repo, actualStart, clientEnd, repoType)
	if err != nil {
		return err
	}

	resp, err := m.sendDownloadRequest(req, c, owner, repo, repoType)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	totalFileSize := m.parseTotalFileSize(resp)

	cacheFile, fileLock, err := m.openCacheFile(cachePath, c)
	if err != nil {
		return err
	}
	defer func() {
		if cacheFile != nil {
			if closeErr := cacheFile.Close(); closeErr != nil {
				zap.S().Errorf("关闭缓存文件失败: %s, err: %v", cachePath, closeErr)
			}
		}
		if fileLock != nil {
			if unlockErr := fileLock.Unlock(); unlockErr != nil {
				zap.S().Errorf("解锁缓存文件锁失败: %s, err: %v", cachePath+".lock", unlockErr)
			}
		}
	}()

	headerWritten = m.writeResponseHeader(c, resp, headerWritten)

	err = m.copyResponseToCacheAndClient(c, resp, cacheFile, cachePath, totalFileSize)
	if err != nil {
		return err
	}

	return nil
}

// buildDownloadRequest 构建下载剩余部分的HTTP请求
func (m *ModelscopeService) buildDownloadRequest(c echo.Context, owner, repo string, actualStart, clientEnd int64, repoType string) (*http.Request, error) {
	apiPrefix := util.GetAPIPathPrefix(repoType)
	query := c.Request().URL.RawQuery
	officialURL := fmt.Sprintf("%s/api/v1/%s/%s/%s/repo?%s",
		config.SysConfig.Modelscope.OfficialBaseURL,
		apiPrefix,
		url.PathEscape(owner),
		url.PathEscape(repo),
		query,
	)
	zap.S().Infof("请求ModelScope官方地址: %s", officialURL)

	req, err := http.NewRequest(http.MethodGet, officialURL, nil)
	if err != nil {
		zap.S().Errorf("构建请求失败: %v", err)
		return nil, c.JSON(http.StatusInternalServerError, map[string]string{
			"code":  "500",
			"error": "build request failed",
			"msg":   err.Error(),
		})
	}

	skipHeaders := map[string]bool{
		"Range":      true,
		"User-Agent": true,
		"Host":       true,
	}
	for k, v := range c.Request().Header {
		key := strings.ToLower(k)
		if !skipHeaders[key] {
			req.Header[k] = v
		}
	}

	util.AddCLIHeaders(req.Header, c.Request().Header.Get("User-Agent"))

	rangeHeader := fmt.Sprintf("bytes=%d-", actualStart)
	if clientEnd != -1 {
		rangeHeader = fmt.Sprintf("bytes=%d-%d", actualStart, clientEnd)
	}
	req.Header.Set("Range", rangeHeader)
	zap.S().Infof("向官方请求剩余部分: %s", rangeHeader)

	return req, nil
}

// sendDownloadRequest 发送下载请求并校验响应状态码
func (m *ModelscopeService) sendDownloadRequest(req *http.Request, c echo.Context, owner, repo, repoType string) (*http.Response, error) {
	resp, err := util.DoRequestWithRetry(req)
	if err != nil {
		zap.S().Errorf("下载剩余部分失败: %v", err)
		return nil, c.JSON(http.StatusBadGateway, map[string]string{
			"code":  "502",
			"error": "download remaining failed",
			"msg":   err.Error(),
		})
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		officialURL := req.URL.String()
		zap.S().Errorf("ModelScope返回错误状态码: %d, URL: %s", resp.StatusCode, officialURL)
		errorMsg := fmt.Sprintf("modelscope server return status code: %d", resp.StatusCode)

		var respJSON error
		switch resp.StatusCode {
		case http.StatusNotFound:
			respJSON = c.JSON(http.StatusNotFound, map[string]string{
				"code":  "404",
				"error": "resource not found",
				"msg":   "model or file does not exist on ModelScope",
			})
		case http.StatusForbidden:
			respJSON = c.JSON(http.StatusForbidden, map[string]string{
				"code":  "403",
				"error": "forbidden",
				"msg":   "no permission to access the resource",
			})
		default:
			respJSON = c.JSON(http.StatusBadGateway, map[string]string{
				"code":  "502",
				"error": "modelscope server error",
				"msg":   errorMsg,
			})
		}
		return nil, respJSON
	}

	return resp, nil
}

// parseTotalFileSize 从响应头解析Content-Range获取总文件大小
func (m *ModelscopeService) parseTotalFileSize(resp *http.Response) int64 {
	totalFileSize := int64(-1)
	contentRange := resp.Header.Get("Content-Range")
	if contentRange != "" {
		parts := strings.Split(contentRange, "/")
		if len(parts) == 2 {
			parsedSize, err := strconv.ParseInt(parts[1], 10, 64)
			if err == nil {
				totalFileSize = parsedSize
			} else {
				zap.S().Warnf("解析Content-Range失败: %s, err: %v", contentRange, err)
			}
		}
	}
	return totalFileSize
}

// writeResponseHeader 写入续传响应头
func (m *ModelscopeService) writeResponseHeader(c echo.Context, resp *http.Response, headerWritten bool) bool {
	if !headerWritten {
		if resp.StatusCode == http.StatusPartialContent {
			c.Response().Header().Set("Content-Range", resp.Header.Get("Content-Range"))
			c.Response().WriteHeader(http.StatusPartialContent)
		} else {
			c.Response().WriteHeader(http.StatusOK)
		}
		headerWritten = true
		zap.S().Infof("设置续传响应头，状态码: %d", resp.StatusCode)
	}
	return headerWritten
}
