package controller

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"log"
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
	batchMaxWait         time.Duration
	batchDataList        []BatchData
	InputQueue           workqueue.RateLimitingInterface
	ResultQueue          workqueue.RateLimitingInterface
	checkBatchDataLength chan bool
	dataType             string
	functionInvoker      executor.StreamingFunctionRunner
	watchdogConfig       config.WatchdogConfig
	rwMutex              sync.RWMutex
}

type Env struct {
	Header           string
	RawQuery         string
	Path             string
	TransferEncoding string
}

type BatchData struct {
	Envs   Env
	Inputs string
	Uuid   string
}

// NewBatchingController creates a new BatchingController
func NewBatchingController(batchSize int, batchMaxWait time.Duration, dataType string, inputQueue workqueue.RateLimitingInterface, resultQueue workqueue.RateLimitingInterface, functionInvoker executor.StreamingFunctionRunner, watchdogConfig config.WatchdogConfig) *BatchingController {
	return &BatchingController{
		batchSize:            batchSize,
		batchMaxWait:         batchMaxWait,
		batchDataList:        make([]BatchData, 0),
		InputQueue:           inputQueue,
		ResultQueue:          resultQueue,
		checkBatchDataLength: make(chan bool),
		dataType:             dataType,
		functionInvoker:      functionInvoker,
		watchdogConfig:       watchdogConfig,
		rwMutex:              sync.RWMutex{},
	}
}

func (c *BatchingController) checkBatchDataMapLength() {
	for {
		length := len(c.batchDataList)
		if length >= c.batchSize {
			c.checkBatchDataLength <- true
		}
		c.checkBatchDataLength <- false
	}
}

// Start starts the batching controller
func (c *BatchingController) Start(threadiness int, stopCh <-chan struct{}) {
	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
		go wait.Until(c.checkBatchDataMapLength, time.Second, stopCh)
	}
	for {
		select {
		case <-stopCh:
			return
		case <-time.After(c.batchMaxWait):
			c.processBatch()
		case <-c.checkBatchDataLength:
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
		if err := json.Unmarshal([]byte(obj.(string)), &data); err != nil {
			c.InputQueue.Forget(obj)
			klog.Infof("BatchingController: addInputsToBatchDataMap: error: %v", err)
			return nil
		}
		c.batchDataList = append(c.batchDataList, data)
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
	for _, data := range batchDataList {
		var inputRequest map[string]map[string]string
		if err := json.Unmarshal([]byte(data.Inputs), &inputRequest); err == nil {
			if inputs, ok := inputRequest["inputs"]; ok {
				for k, v := range inputs {
					batchData[k] = v
				}
			}
			env := data.Envs
			batchRequest[env] = batchingDataMap
		}
	}
	var wg sync.WaitGroup

	for env, data := range batchRequest {
		wg.Add(1)
		go func(env Env, data map[string]map[string]string) {
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
				environment = append(environment, env.TransferEncoding)
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
			c.ResultQueue.Add(buf.String())
		}(env, data)
	}
	wg.Wait()
}
