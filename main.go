package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"mime"
	"net/http"
	"net/http/httputil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ruyewuxin/bar"
)

const (
	//DEFAULT_DOWNLOAD_BLOCK 1M
	DEFAULT_DOWNLOAD_BLOCK int64 = 1 << 20
)

var ostype = runtime.GOOS

//GoGet ...
type GoGet struct {
	URL           string //下载URL
	Cnt           int    //下载线程
	DownloadBlock int64
	CostomCnt     int
	Latch         int
	Header        http.Header
	MediaType     string
	MediaParams   map[string]string
	FilePath      string // 包括路径和文件名
	GetClient     *http.Client
	ContentLength int64
	DownloadRange [][]int64
	File          *os.File
	TempFiles     []*os.File
	WG            sync.WaitGroup
	lock          sync.Mutex //互斥锁
	cur           int
	bar           bar.Bar //进度条
}

//GetCurPath 获取当前文件执行的路径
func GetCurPath() string {
	file, _ := exec.LookPath(os.Args[0])
	//得到全路径，比如在windows下E:\\golang\\test\\a.exe
	path, _ := filepath.Abs(file)
	rst := filepath.Dir(path)

	return rst
}

//NewGoGet ...
func NewGoGet() *GoGet {
	flag.Parse()

	get := new(GoGet)
	get.GetClient = new(http.Client)
	get.URL = *urlFlag
	urlSlice := strings.Split(get.URL, "/")

	if ostype == "windows" {
		get.FilePath = GetCurPath() + "\\" + urlSlice[len(urlSlice)-1]
	} else if ostype == "linux" {
		get.FilePath = GetCurPath() + "/" + urlSlice[len(urlSlice)-1]
	}

	get.DownloadBlock = DEFAULT_DOWNLOAD_BLOCK

	return get
}

var urlFlag = flag.String("u", "https://www.baidu.com/img/fddong_e2dd633ee46695630e60156c91cda80a.gif", "Fetch file url")

// var cntFlag = flag.Int("c", 1, "Fetch concurrently counts")

func main() {
	get := NewGoGet()

	downloadStartTime := time.Now()

	req, err := http.NewRequest("HEAD", get.URL, nil)
	if err != nil {
		log.Panicf("Get request error %v.\n", err)
	}
	resp, err := get.GetClient.Do(req)
	// resp, err := http.Get(get.URL)

	if err != nil {
		log.Panicf("Get %s error %v.\n", get.URL, err)
	}

	get.Header = resp.Header
	get.MediaType, get.MediaParams, _ = mime.ParseMediaType(get.Header.Get("Content-Disposition"))
	get.ContentLength = resp.ContentLength
	get.Cnt = int(math.Ceil(float64(get.ContentLength/get.DownloadBlock))) + 1
	if strings.HasSuffix(get.FilePath, "/") {
		get.FilePath += get.MediaParams["filename"]
	}
	get.File, err = os.Create(get.FilePath)
	if err != nil {
		log.Panicf("Create file %s error %v.\n", get.FilePath, err)
	}

	log.Printf("Get %s MediaType:%s, Filename:%s, Size %d.\n", get.URL, get.MediaType, get.MediaParams["filename"], get.ContentLength)
	if get.Header.Get("Accept-Ranges") != "" {
		log.Printf("Server %s support Range by %s.\n", get.Header.Get("Server"), get.Header.Get("Accept-Ranges"))
	} else {
		log.Printf("Server %s doesn't support Range.\n", get.Header.Get("Server"))
	}

	log.Printf("Start to download %s with %d thread.\n", get.MediaParams["filename"], get.Cnt)
	var rangeStart int64 = 0
	for i := 0; i < get.Cnt; i++ {
		if i != get.Cnt-1 {
			get.DownloadRange = append(get.DownloadRange, []int64{rangeStart, rangeStart + get.DownloadBlock - 1})
		} else {
			// 最后一块
			get.DownloadRange = append(get.DownloadRange, []int64{rangeStart, get.ContentLength - 1})
		}
		rangeStart += get.DownloadBlock
	}

	// Check if the download has paused.
	for i := 0; i < len(get.DownloadRange); i++ {
		rangeI := fmt.Sprintf("%d-%d", get.DownloadRange[i][0], get.DownloadRange[i][1])
		tempFile, err := os.OpenFile(get.FilePath+"."+rangeI, os.O_RDONLY|os.O_APPEND, 0)
		if err != nil {
			tempFile, _ = os.Create(get.FilePath + "." + rangeI)
			// tempFile.Close()
		} else {
			fi, err := tempFile.Stat()
			if err == nil {
				get.DownloadRange[i][0] += fi.Size()
			}
		}
		get.TempFiles = append(get.TempFiles, tempFile)
	}

	get.bar.NewOption(0, get.Cnt)
	get.Latch = get.Cnt
	for i := range get.DownloadRange {
		get.WG.Add(1)
		go get.Download(i)
	}

	get.WG.Wait()
	get.bar.Finish()
	get.Merge()

	log.Printf("Download complete and store file %s with %v.\n", get.FilePath, time.Since(downloadStartTime))
	defer func() {
		for i := 0; i < len(get.TempFiles); i++ {
			err := os.Remove(get.TempFiles[i].Name())
			if err != nil {
				log.Printf("Remove temp file %s error %v.\n", get.TempFiles[i].Name(), err)
			} else {
				// log.Printf("Remove temp file %s.\n", get.TempFiles[i].Name())
			}
		}
	}()
}

//Download ...
func (get *GoGet) Download(i int) {
	defer get.WG.Done()
	if get.DownloadRange[i][0] > get.DownloadRange[i][1] {
		return
	}
	rangeI := fmt.Sprintf("%d-%d", get.DownloadRange[i][0], get.DownloadRange[i][1])

	defer get.TempFiles[i].Close()

	req, err := http.NewRequest("GET", get.URL, nil)
	if err != nil {
		log.Printf("Download #%d bytes %s.\n", i, rangeI)
	}
	req.Header.Set("Range", "bytes="+rangeI)
	resp, err := get.GetClient.Do(req)

	if err != nil {
		log.Printf("Download #%d error %v.\n", i, err)
	} else {
		cnt, err := io.Copy(get.TempFiles[i], resp.Body)
		if cnt == int64(get.DownloadRange[i][1]-get.DownloadRange[i][0]+1) {
			// log.Printf("Download #%d complete.\n", i)
		} else {
			reqDump, _ := httputil.DumpRequest(req, false)
			respDump, _ := httputil.DumpResponse(resp, true)
			log.Panicf("Download error %d %v, expect %d-%d, but got %d.\nRequest: %s\nResponse: %s\n", resp.StatusCode, err, get.DownloadRange[i][0], get.DownloadRange[i][1], cnt, string(reqDump), string(respDump))
		}
	}
	defer resp.Body.Close()
	get.lock.Lock() //加锁
	get.cur++
	get.Watch()
	get.lock.Unlock() //解锁
}

//Watch ...
func (get *GoGet) Watch() {
	get.bar.Play(get.cur)
}

//Merge ...
func (get *GoGet) Merge() {
	defer get.File.Close()
	for i := 0; i < len(get.TempFiles); i++ {
		tempFile, _ := os.Open(get.TempFiles[i].Name())
		cnt, err := io.Copy(get.File, tempFile)
		if cnt <= 0 || err != nil {
			log.Printf("Merge #%d error %v.\n", i, err)
		}
		tempFile.Close()
		get.TempFiles[i].Close()
	}
}
