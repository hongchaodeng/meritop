package framework

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"os"
	"strconv"
	"sync"

	"github.com/go-distributed/meritop"
)

/*
The dummy task is designed for regresion test of meritop framework.
This works with tree topology.
The main idea behind the regression test is following:
There will be two kinds of dummyTasks: master and slaves. We will have one master
sits at the top with taskID = 0, and then rest 6 (2^n - 2) tasks forms a tree under
the master. There will be 10 epochs, from 1 to 10, at each epoch, we send out a
vector with all values equal to epochID, and each slave is supposedly return a vector
with all values equals epochID*taskID, the values are reduced back to master, and
master will print out the epochID and aggregated vector. After all 10 epoch, it kills
job.
*/

// dummyData is used to carry parameter and gradient;
type dummyData struct {
	Value int32
}

// dummyMaster is prototype of parameter server, for now it does not
// carry out optimization yet. But it should be easy to add support when
// this full tests out.
// Note: in theory, since there should be no parent of this, so we should
// add error checing in the right places. We will skip these test for now.
type dummyMaster struct {
	dataChan           chan int32
	finishChan         chan struct{}
	NodeProducer       chan bool
	framework          meritop.Framework
	epoch, taskID      uint64
	logger             *log.Logger
	config             map[string]string
	numberOfIterations uint64

	param, gradient *dummyData
	fromChildren    map[uint64]*dummyData
}

// This is useful to bring the task up to speed from scratch or if it recovers.
func (t *dummyMaster) Init(taskID uint64, framework meritop.Framework) {
	t.taskID = taskID
	t.framework = framework
	t.logger = log.New(os.Stdout, "", log.Ldate|log.Ltime|log.Lshortfile)
	// t.logger = log.New(ioutil.Discard, "", log.Ldate|log.Ltime|log.Lshortfile)
}

// Task need to finish up for exit, last chance to save work?
func (t *dummyMaster) Exit() {}

// Ideally, we should also have the following:
func (t *dummyMaster) ParentMetaReady(ctx meritop.Context, parentID uint64, meta string) {}
func (t *dummyMaster) ChildMetaReady(ctx meritop.Context, childID uint64, meta string) {
	t.logger.Printf("master ChildMetaReady, task: %d, epoch: %d, child: %d\n", t.taskID, t.epoch, childID)
	// Get data from child. When all the data is back, starts the next epoch.
	ctx.DataRequest(childID, meta)
}

// This give the task an opportunity to cleanup and regroup.
func (t *dummyMaster) SetEpoch(ctx meritop.Context, epoch uint64) {
	t.logger.Printf("master SetEpoch, task: %d, epoch: %d\n", t.taskID, epoch)
	if t.testablyFail("SetEpoch", strconv.FormatUint(epoch, 10)) {
		return
	}

	t.param = &dummyData{}
	t.gradient = &dummyData{}

	t.epoch = epoch
	t.param.Value = int32(t.epoch)

	// Make sure we have a clean slate.
	t.fromChildren = make(map[uint64]*dummyData)
	ctx.FlagMetaToChild("ParamReady")
}

// These are payload rpc for application purpose.
func (t *dummyMaster) ServeAsParent(fromID uint64, req string) []byte {
	b, err := json.Marshal(t.param)
	if err != nil {
		t.logger.Fatalf("Master can't encode parameter: %v, error: %v\n", t.param, err)
	}
	return b
}

func (t *dummyMaster) ServeAsChild(fromID uint64, req string) []byte {
	return nil
}

func (t *dummyMaster) ParentDataReady(ctx meritop.Context, parentID uint64, req string, resp []byte) {}
func (t *dummyMaster) ChildDataReady(ctx meritop.Context, childID uint64, req string, resp []byte) {
	d := new(dummyData)
	json.Unmarshal(resp, d)
	if _, ok := t.fromChildren[childID]; ok {
		return
	}
	t.fromChildren[childID] = d

	t.logger.Printf("master ChildDataReady, task: %d, epoch: %d, child: %d, ready: %d\n",
		t.taskID, t.epoch, childID, len(t.fromChildren))

	// This is a weak form of checking. We can also check the task ids.
	// But this really means that we get all the events from children, we
	// should go into the next epoch now.
	if len(t.fromChildren) == len(t.framework.GetTopology().GetChildren(t.epoch)) {
		for _, g := range t.fromChildren {
			t.gradient.Value += g.Value
		}

		t.dataChan <- t.gradient.Value
		// TODO(xiaoyunwu) We need to do some test here.

		// In real ML, we modify the gradient first. But here it is noop.
		if t.epoch == t.numberOfIterations {
			if t.config["writefile"] != "" {
				data := []byte(fmt.Sprintf("Finished job. Gradient value: %v\n", t.gradient.Value))
				ioutil.WriteFile(t.config["writefile"], data, 0644)
			}
			t.framework.ShutdownJob()
			close(t.finishChan)
		} else {
			t.logger.Printf("master finished current epoch, task: %d, epoch: %d", t.taskID, t.epoch)
			ctx.IncEpoch()
		}
	}
}

func (t *dummyMaster) testablyFail(method string, args ...string) bool {
	if t.config == nil {
		return false
	}
	if t.config[method] != "fail" {
		return false
	}
	if len(args) >= 1 && t.config["failepoch"] != "" {
		// we need to care about fail at specific epoch
		if t.config["failepoch"] != args[0] {
			return false
		}
	}
	if !probablyFail(t.config["faillevel"]) {
		return false
	}
	t.logger.Printf("master task %d testably fail, method: %s\n", t.taskID, method)
	t.framework.(*framework).stop()
	t.NodeProducer <- true
	return true
}

// dummySlave is an prototype for data shard in machine learning applications.
// It mainly does to things, pass on parameters to its children, and collect
// gradient back then add them together before make it available to its parent.
type dummySlave struct {
	framework     meritop.Framework
	epoch, taskID uint64
	logger        *log.Logger
	NodeProducer  chan bool
	config        map[string]string

	param, gradient *dummyData
	fromChildren    map[uint64]*dummyData
	gradientReady   *countDownLatch
}

// This is useful to bring the task up to speed from scratch or if it recovers.
func (t *dummySlave) Init(taskID uint64, framework meritop.Framework) {
	t.taskID = taskID
	t.framework = framework
	t.logger = log.New(os.Stdout, "", log.Ldate|log.Ltime|log.Lshortfile)
	// t.logger = log.New(ioutil.Discard, "", log.Ldate|log.Ltime|log.Lshortfile)
}

// Task need to finish up for exit, last chance to save work?
func (t *dummySlave) Exit() {}

// Ideally, we should also have the following:
func (t *dummySlave) ParentMetaReady(ctx meritop.Context, parentID uint64, meta string) {
	t.logger.Printf("slave ParentMetaReady, task: %d, epoch: %d\n", t.taskID, t.epoch)
	ctx.DataRequest(parentID, meta)
}

func (t *dummySlave) ChildMetaReady(ctx meritop.Context, childID uint64, meta string) {
	t.logger.Printf("slave ChildMetaReady, task: %d, epoch: %d\n", t.taskID, t.epoch)
	ctx.DataRequest(childID, meta)
}

// This give the task an opportunity to cleanup and regroup.
func (t *dummySlave) SetEpoch(ctx meritop.Context, epoch uint64) {
	t.logger.Printf("slave SetEpoch, task: %d, epoch: %d\n", t.taskID, epoch)
	t.param = &dummyData{}
	t.gradient = &dummyData{}
	t.gradientReady = newCountDownLatch(1)

	t.epoch = epoch
	// Make sure we have a clean slate.
	t.fromChildren = make(map[uint64]*dummyData)
}

// These are payload rpc for application purpose.
func (t *dummySlave) ServeAsParent(fromID uint64, req string) []byte {
	b, err := json.Marshal(t.param)
	if err != nil {
		t.logger.Fatalf("Slave can't encode parameter: %v, error: %v\n", t.param, err)
	}
	return b
}

func (t *dummySlave) ServeAsChild(fromID uint64, req string) []byte {
	b, err := json.Marshal(t.gradient)
	if err != nil {
		t.logger.Fatalf("Slave can't encode gradient: %v, error: %v\n", t.gradient, err)
	}
	return b
}

func (t *dummySlave) ParentDataReady(ctx meritop.Context, parentID uint64, req string, resp []byte) {
	t.logger.Printf("slave ParentDataReady, task: %d, epoch: %d, parent: %d\n", t.taskID, t.epoch, parentID)
	if t.testablyFail("ParentDataReady") {
		return
	}
	if t.gradientReady.Count() == 0 {
		return
	}
	t.param = new(dummyData)
	json.Unmarshal(resp, t.param)
	// We need to carry out local compuation.
	t.gradient.Value = t.param.Value * int32(t.framework.GetTaskID())
	t.gradientReady.CountDown()

	// If this task has children, flag meta so that children can start pull
	// parameter.
	children := t.framework.GetTopology().GetChildren(t.epoch)
	if len(children) != 0 {
		ctx.FlagMetaToChild("ParamReady")
	} else {
		// On leaf node, we can immediately return by and flag parent
		// that this node is ready.
		ctx.FlagMetaToParent("GradientReady")
	}
}

func (t *dummySlave) ChildDataReady(ctx meritop.Context, childID uint64, req string, resp []byte) {
	d := new(dummyData)
	json.Unmarshal(resp, d)
	if _, ok := t.fromChildren[childID]; ok {
		return
	}
	t.fromChildren[childID] = d

	t.logger.Printf("slave ChildDataReady, task: %d, epoch: %d, child: %d, ready: %d\n",
		t.taskID, t.epoch, childID, len(t.fromChildren))

	// This is a weak form of checking. We can also check the task ids.
	// But this really means that we get all the events from children, we
	// should go into the next epoch now.
	if len(t.fromChildren) == len(t.framework.GetTopology().GetChildren(t.epoch)) {
		// If a new node restart and find out both parent and child meta ready, it will
		// simultaneously request both data. We need to wait until gradient data is there.
		t.gradientReady.Await()
		// In real ML, we add the gradient first.
		for _, g := range t.fromChildren {
			t.gradient.Value += g.Value
		}

		// If this failure happens, a new node will redo computing again.
		if t.testablyFail("ChildDataReady") {
			return
		}

		ctx.FlagMetaToParent("GradientReady")

		// if this failure happens, the parent could
		// 1. not have the data yet. In such case, the parent could
		//   1.1 not request the data before a new node restarts. This will cause
		//       double requests since we provide at-least-once semantics.
		//   1.2 request the data with a failed host (request should fail or be
		//       responded with error message).
		// 2. already get the data.
		if t.testablyFail("ChildDataReady") {
			return
		}
	}
}

func (t *dummySlave) testablyFail(method string, args ...string) bool {
	if t.config == nil {
		return false
	}
	if t.config[method] != "fail" {
		return false
	}
	if !probablyFail(t.config["faillevel"]) {
		return false
	}
	t.logger.Printf("slave task %d testably fail, method: %s\n", t.taskID, method)
	t.framework.(*framework).stop()
	t.NodeProducer <- true
	return true
}

func probablyFail(levelStr string) bool {
	level, err := strconv.Atoi(levelStr)
	if err != nil {
		return false
	}
	if level < rand.Intn(100)+1 {
		return false
	}
	return true
}

// used for testing
type SimpleTaskBuilder struct {
	GDataChan          chan int32
	FinishChan         chan struct{}
	NumberOfIterations uint64
	NodeProducer       chan bool
	MasterConfig       map[string]string
	SlaveConfig        map[string]string
}

// This method is called once by framework implementation to get the
// right task implementation for the node/task. It requires the taskID
// for current node, and also a global array of tasks.
func (tc SimpleTaskBuilder) GetTask(taskID uint64) meritop.Task {
	if taskID == 0 {
		return &dummyMaster{
			dataChan:           tc.GDataChan,
			finishChan:         tc.FinishChan,
			NodeProducer:       tc.NodeProducer,
			config:             tc.MasterConfig,
			numberOfIterations: tc.NumberOfIterations,
		}
	}
	return &dummySlave{
		NodeProducer: tc.NodeProducer,
		config:       tc.SlaveConfig,
	}
}

// I am writing this count down latch because sync.WaitGroup doesn't support
// decrementing counter when it's 0.
type countDownLatch struct {
	sync.Mutex
	cond    *sync.Cond
	counter int
}

func newCountDownLatch(count int) *countDownLatch {
	c := new(countDownLatch)
	c.cond = sync.NewCond(c)
	c.counter = count
	return c
}

func (c *countDownLatch) Count() int {
	c.Lock()
	defer c.Unlock()
	return c.counter
}

func (c *countDownLatch) CountDown() {
	c.Lock()
	defer c.Unlock()
	if c.counter == 0 {
		return
	}
	c.counter--
	if c.counter == 0 {
		c.cond.Broadcast()
	}
}

func (c *countDownLatch) Await() {
	c.Lock()
	defer c.Unlock()
	if c.counter == 0 {
		return
	}
	c.cond.Wait()
}
