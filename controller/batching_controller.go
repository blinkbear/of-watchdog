package controller

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"log"
	"os"
	"sync"
	"time"

	"github.com/openfaas/of-watchdog/config"
	"github.com/openfaas/of-watchdog/executor"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

// Batch Data Map [{"inputs":{"demo":"input"}}]

type BatchingController struct {
	batchSize            int
	batchWaitTimeout     time.Duration
	batchDataList        []BatchData
	InputQueue           workqueue.RateLimitingInterface
	checkBatchDataLength chan bool
	functionInvoker      executor.StreamingFunctionRunner
	watchdogConfig       config.WatchdogConfig
	rwMutex              sync.RWMutex
}

type Env struct {
	Header           string
	RawQuery         string
	Path             string
	Method           string
	TransferEncoding string
}

type BatchData struct {
	Envs       Env
	Inputs     string
	ResultChan chan string
}

// NewBatchingController creates a new BatchingController
func NewBatchingController(watchdogConfig config.WatchdogConfig, inputQueue workqueue.RateLimitingInterface, resultQueue workqueue.RateLimitingInterface) *BatchingController {
	functionInvoker := executor.StreamingFunctionRunner{
		ExecTimeout:   watchdogConfig.ExecTimeout,
		LogPrefix:     watchdogConfig.PrefixLogs,
		LogBufferSize: watchdogConfig.LogBufferSize,
	}
	return &BatchingController{
		batchSize:            watchdogConfig.BatchSize,
		batchWaitTimeout:     watchdogConfig.BatchWaitTimeout,
		batchDataList:        make([]BatchData, 0),
		InputQueue:           inputQueue,
		checkBatchDataLength: make(chan bool),
		functionInvoker:      functionInvoker,
		watchdogConfig:       watchdogConfig,
		rwMutex:              sync.RWMutex{},
	}
}

func (c *BatchingController) checkBatchDataMapLength() {
	length := len(c.batchDataList)
	if length >= c.batchSize {
		c.checkBatchDataLength <- true
	}
	c.checkBatchDataLength <- false
}

// Run the batching controller
func (c *BatchingController) Run(stopCh <-chan struct{}) {
	defer c.InputQueue.ShutDown()
	for i := 0; i < c.watchdogConfig.Threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)

	}
	go wait.Until(c.checkBatchDataMapLength, 50*time.Millisecond, stopCh)
	for {
		select {
		case <-stopCh:
			klog.Info("Shutting down batching controller")
			return
		case <-time.After(c.batchWaitTimeout):
			klog.Info("BatchingController: batchWaitTimeout")
			c.processBatch()
		case <-c.checkBatchDataLength:
			klog.Info("BatchingController: checkBatchDataLength")
			c.processBatch()
		}
	}
}

func (c *BatchingController) runWorker() {
	for c.addInputsToBatchDataMap() {

	}
}

func (c *BatchingController) AddBatchData(newData BatchData) {
	c.rwMutex.Lock()
	defer c.rwMutex.Unlock()
	c.batchDataList = append(c.batchDataList, newData)
}

func (c *BatchingController) GetBatchData() []BatchData {
	c.rwMutex.Lock()
	defer c.rwMutex.Unlock()
	batchDataList := c.batchDataList
	c.batchDataList = make([]BatchData, 0)
	return batchDataList
}

func (c *BatchingController) addInputsToBatchDataMap() bool {
	klog.Infof("BatchingController: addInputsToBatchDataMap")
	obj, shutdown := c.InputQueue.Get()
	if shutdown {
		klog.Infof("BatchingController: addInputsToBatchDataMap: shutdown")
		return false
	}
	err := func(obj interface{}) error {
		defer c.InputQueue.Done(obj)
		var data BatchData
		var ok bool
		if data, ok = obj.(BatchData); !ok {
			c.InputQueue.Forget(obj)
			klog.Infof("BatchingController: addInputsToBatchDataMap: error: got %v", obj)
			return nil
		}
		c.AddBatchData(data)
		c.InputQueue.Forget(obj)
		return nil
	}(obj)
	if err != nil {
		klog.Infof("BatchingController: addInputsToBatchDataMap: error: %v", err)
		return true
	}
	return true

}

func (c *BatchingController) processBatch() {
	batchRequest := make(map[Env]map[string]map[string]string)
	batchData := make(map[string]string)
	batchingDataMap := map[string]map[string]string{}
	batchingDataMap["inputs"] = batchData
	batchDataList := c.GetBatchData()
	channelList := make([]chan string, 0)
	if len(batchDataList) == 0 {
		return
	}
	for _, data := range batchDataList {
		var inputRequest map[string]map[string]string
		if err := json.Unmarshal([]byte(data.Inputs), &inputRequest); err == nil {
			if inputs, ok := inputRequest["inputs"]; ok {
				for k, v := range inputs {
					batchData[k] = v
				}
			}
			channelList = append(channelList, data.ResultChan)
			env := data.Envs
			batchRequest[env] = batchingDataMap
		}
	}
	var wg sync.WaitGroup

	for env, data := range batchRequest {
		wg.Add(1)
		go func(env Env, data map[string]map[string]string, channelList []chan string) {
			defer wg.Done()
			var buf bytes.Buffer
			requests, ok := json.Marshal(data)
			if ok != nil {
				return
			}
			requestReader := ioutil.NopCloser(bytes.NewReader(requests))
			environment := []string{}
			if c.watchdogConfig.InjectCGIHeaders {
				environment = append(environment, env.Header)
				environment = append(environment, env.RawQuery)
				environment = append(environment, env.Path)
				environment = append(environment, env.Method)
				environment = append(environment, env.TransferEncoding)
				os_env := os.Environ()
				environment = append(environment, os_env...)
			}
			commandName, arguments := c.watchdogConfig.Process()
			req := executor.FunctionRequest{
				Process:      commandName,
				ProcessArgs:  arguments,
				InputReader:  requestReader,
				OutputWriter: &buf,
				Environment:  environment,
			}
			err := c.functionInvoker.Run(req)
			if err != nil {
				log.Println(err.Error())
			}
			result := buf.String()
			for _, v := range data {
				resultLength := len(v)
				for i := 0; i < resultLength; i++ {
					channelList[i] <- result
				}
			}

		}(env, data, channelList)
	}
	wg.Wait()
}
