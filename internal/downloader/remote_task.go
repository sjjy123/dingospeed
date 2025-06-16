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

package downloader

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"

	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	"dingospeed/pkg/prom"
	"dingospeed/pkg/util"

	"go.uber.org/zap"
)

type RemoteFileTask struct {
	DownloadTask
	authorization string
	hfUrl         string
	Queue         chan []byte `json:"-"`
}

func NewRemoteFileTask(taskNo int, rangeStartPos int64, rangeEndPos int64) *RemoteFileTask {
	r := &RemoteFileTask{}
	r.TaskNo = taskNo
	r.RangeStartPos = rangeStartPos
	r.RangeEndPos = rangeEndPos
	return r
}

// 预分配池以减少GC压力
var bufferPool = &sync.Pool{
	New: func() interface{} {
		return &bytes.Buffer{}
	},
}

func (r RemoteFileTask) DoTask() {
	var (
		curBlock int64
		wg       sync.WaitGroup
	)

	contentChan := make(chan []byte, consts.RespChanSize)
	rangeStartPos, rangeEndPos := r.RangeStartPos, r.RangeEndPos
	zap.S().Infof("remote file download:%s/%s, taskNo:%d, size:%d, startPos:%d, endPos:%d",
		r.orgRepo, r.FileName, r.TaskNo, r.TaskSize, rangeStartPos, rangeEndPos)

	wg.Add(2)
	go r.getFileRangeFromRemote(&wg, rangeStartPos, rangeEndPos, contentChan)

	curPos := rangeStartPos
	streamCache := bufferPool.Get().(*bytes.Buffer)
	streamCache.Reset()
	defer bufferPool.Put(streamCache)

	lastBlock, lastBlockStartPos, lastBlockEndPos := getBlockInfo(curPos, r.DingFile.getBlockSize(), r.DingFile.GetFileSize())

	go func() {
		defer func() {
			close(r.Queue)
			wg.Done()
		}()

		for {
			select {
			case chunk, ok := <-contentChan:
				if !ok {
					return
				}

				// 处理并转发数据块
				select {
				case r.Queue <- chunk:
				case <-r.Context.Done():
					return
				}

				curPos += int64(len(chunk))
				streamCache.Write(chunk)

				if config.SysConfig.EnableMetric() {
					source := util.Itoa(r.Context.Value(consts.PromSource))
					prom.PromRequestByteCounter(prom.RequestRemoteByte, source, int64(len(chunk)))
				}

				curBlock = curPos / r.DingFile.getBlockSize()
				if curBlock != lastBlock {
					splitPos := lastBlockEndPos - max(lastBlockStartPos, rangeStartPos)
					cacheLen := int64(streamCache.Len())

					if splitPos > cacheLen {
						zap.S().Errorf("splitPos err.%d-%d", splitPos, cacheLen)
						splitPos = cacheLen
					}

					rawBlock := streamCache.Bytes()[:splitPos]
					if len(rawBlock) == int(r.DingFile.getBlockSize()) {
						if err := r.processBlock(lastBlock, rawBlock); err != nil {
							zap.S().Errorf("processBlock err: %v", err)
						}
					}

					nextBlock := streamCache.Bytes()[splitPos:]
					streamCache.Truncate(0)
					streamCache.Write(nextBlock)
					lastBlock, lastBlockStartPos, lastBlockEndPos = getBlockInfo(curPos, r.DingFile.getBlockSize(), r.DingFile.GetFileSize())
				}
			case <-r.Context.Done():
				zap.S().Warnf("file:%s/%s, task %d, ctx done, DoTask exit.", r.orgRepo, r.FileName, r.TaskNo)
				return
			}
		}
	}()

	wg.Wait()

	// 处理剩余数据
	rawBlock := streamCache.Bytes()
	if curBlock == r.DingFile.getBlockNumber()-1 && len(rawBlock) > 0 {
		// 对不足一个block的数据做补全
		if len(rawBlock) == int(r.DingFile.GetFileSize()%r.DingFile.getBlockSize()) {
			padding := make([]byte, r.DingFile.getBlockSize()-int64(len(rawBlock)))
			rawBlock = append(rawBlock, padding...)
		}

		if len(rawBlock) == int(r.DingFile.getBlockSize()) {
			if err := r.processBlock(curBlock, rawBlock); err != nil {
				zap.S().Errorf("process last block err: %v", err)
			}

			if err := util.CreateSymlinkIfNotExists(r.blobsFile, r.filesPath); err != nil {
				zap.S().Errorf("create symlink err: %v", err)
			}
		}
	}

	if curPos != rangeEndPos {
		zap.S().Warnf("file:%s, taskNo:%d, remote range (%d) is different from sent size (%d).",
			r.FileName, r.TaskNo, rangeEndPos-rangeStartPos, curPos-rangeStartPos)
	}
}

// 辅助方法：处理数据块
func (r RemoteFileTask) processBlock(blockNum int64, data []byte) error {
	hasBlock, err := r.DingFile.HasBlock(blockNum)
	if err != nil {
		return fmt.Errorf("HasBlock err: %w", err)
	}

	if !hasBlock {
		if err := r.DingFile.WriteBlock(blockNum, data); err != nil {
			return fmt.Errorf("WriteBlock err: %w", err)
		}

		zap.S().Debugf("%s/%s, taskNo:%d, block：%d(%d)write done, range：%d-%d.",
			r.orgRepo, r.FileName, r.TaskNo, blockNum, r.DingFile.getBlockNumber(),
			blockNum*r.DingFile.getBlockSize(), (blockNum+1)*r.DingFile.getBlockSize()-1)
	}

	return nil
}

func (r RemoteFileTask) OutResult() {
	for {
		select {
		case data, ok := <-r.Queue:
			if !ok {
				zap.S().Debugf("OutResult r.Queue close %s/%s", r.orgRepo, r.FileName)
				return
			}
			select {
			case r.ResponseChan <- data:
			case <-r.Context.Done():
				zap.S().Debugf("OutResult remote Context.Done() %s/%s", r.orgRepo, r.FileName)
				return
			}
		case <-r.Context.Done():
			zap.S().Debugf("OutResult remote ctx err, fileName:%s/%s,err:%v", r.orgRepo, r.FileName, r.Context.Err())
			return
		}
	}
}

func (r RemoteFileTask) GetResponseChan() chan []byte {
	return r.ResponseChan
}

func (r RemoteFileTask) getFileRangeFromRemote(wg *sync.WaitGroup, startPos, endPos int64, contentChan chan<- []byte) {
	headers := make(map[string]string)
	if r.authorization != "" {
		headers["authorization"] = r.authorization
	}
	headers["range"] = fmt.Sprintf("bytes=%d-%d", startPos, endPos-1)
	defer func() {
		close(contentChan)
		wg.Done()
	}()
	var rawData []byte
	chunkByteLen := 0
	var contentEncoding, contentLengthStr = "", ""

	if err := util.GetStream(r.hfUrl, headers, config.SysConfig.GetReqTimeOut(), func(resp *http.Response) {
		contentEncoding = resp.Header.Get("content-encoding")
		contentLengthStr = resp.Header.Get("content-length")
		for {
			select {
			case <-r.Context.Done():
				zap.S().Warnf("getFileRangeFromRemote Context.Done err :%s", r.FileName)
				return
			default:
				chunk := make([]byte, config.SysConfig.Download.RespChunkSize)
				n, err := resp.Body.Read(chunk)
				if n > 0 {
					if contentEncoding != "" { // 数据有编码，先收集，后面解码
						rawData = append(rawData, chunk[:n]...)
					} else {
						select {
						case contentChan <- chunk[:n]:
						case <-r.Context.Done():
							return
						}
					}
					chunkByteLen += n // 原始数量
				}
				if err != nil {
					if err == io.EOF {
						return
					}
					zap.S().Errorf("file:%s, task %d, req remote err.%v", r.FileName, r.TaskNo, err)
					return
				}
			}
		}
	}); err != nil {
		zap.S().Errorf("GetStream err.%v", err)
		return
	}
	if contentEncoding != "" {
		// 这里需要实现解压缩逻辑
		finalData, err := util.DecompressData(rawData, contentEncoding)
		if err != nil {
			zap.S().Errorf("DecompressData err.%v", err)
			return
		}
		contentChan <- finalData      // 返回解码后的数据流
		chunkByteLen = len(finalData) // 将解码后的长度复制为原理的chunkBytes
	}
	if contentLengthStr != "" {
		contentLength, err := strconv.Atoi(contentLengthStr) // 原始数据长度
		if err != nil {
			zap.S().Errorf("contentLengthStr conv err.%s", contentLengthStr)
			return
		}
		if contentEncoding != "" {
			contentLength = chunkByteLen
		}
		if endPos-startPos != int64(contentLength) {
			zap.S().Errorf("file:%s, taskNo:%d,The content of the response is incomplete. Expected-%d. Accepted-%d", r.FileName, r.TaskNo, endPos-startPos, contentLength)
			return
		}
	}
	if endPos-startPos != int64(chunkByteLen) {
		zap.S().Warnf("file:%s, taskNo:%d,The block is incomplete. Expected-%d. Accepted-%d", r.FileName, r.TaskNo, endPos-startPos, chunkByteLen)
		return
	}
}
