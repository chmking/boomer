package boomer

import (
	"fmt"
	"log"
	"math/rand"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	stateInit     = "ready"
	stateHatching = "hatching"
	stateRunning  = "running"
	stateStopped  = "stopped"
	stateQuitting = "quitting"
)

const (
	slaveReportInterval = 3 * time.Second
	heartbeatInterval   = 1 * time.Second
)

type runner struct {
	hatchType string
	state     string
	tasks     []*Task

	rateLimiter      RateLimiter
	rateLimitEnabled bool
	stats            *requestStats

	numClients int32
	hatchRate  int

	// all running workers(goroutines) will select on this channel.
	// close this channel will stop all running workers.
	stopChan chan bool

	// close this channel will stop all goroutines used in runner.
	closeChan chan bool

	outputs []Output
}

// safeRun runs fn and recovers from unexpected panics.
// it prevents panics from Task.Fn crashing boomer.
func (r *runner) safeRun(fn func()) {
	defer func() {
		// don't panic
		err := recover()
		if err != nil {
			stackTrace := debug.Stack()
			errMsg := fmt.Sprintf("%v", err)
			os.Stderr.Write([]byte(errMsg))
			os.Stderr.Write([]byte("\n"))
			os.Stderr.Write(stackTrace)
		}
	}()
	fn()
}

func (r *runner) addOutput(o Output) {
	r.outputs = append(r.outputs, o)
}

func (r *runner) outputOnStart() {
	size := len(r.outputs)
	if size == 0 {
		return
	}
	wg := sync.WaitGroup{}
	wg.Add(size)
	for _, output := range r.outputs {
		go func(o Output) {
			o.OnStart()
			wg.Done()
		}(output)
	}
	wg.Wait()
}

func (r *runner) outputOnEevent(data map[string]interface{}) {
	size := len(r.outputs)
	if size == 0 {
		return
	}
	wg := sync.WaitGroup{}
	wg.Add(size)
	for _, output := range r.outputs {
		go func(o Output) {
			o.OnEvent(data)
			wg.Done()
		}(output)
	}
	wg.Wait()
}

func (r *runner) outputOnStop() {
	size := len(r.outputs)
	if size == 0 {
		return
	}
	wg := sync.WaitGroup{}
	wg.Add(size)
	for _, output := range r.outputs {
		go func(o Output) {
			o.OnStop()
			wg.Done()
		}(output)
	}
	wg.Wait()
}

func (r *runner) getWeightSum() (weightSum int) {
	for _, task := range r.tasks {
		weightSum += task.Weight
	}
	return weightSum
}

func (r *runner) spawnWorkers(spawnCount int, quit chan bool, hatchCompleteFunc func()) {
	log.Println("Hatching and swarming", spawnCount, "clients at the rate", r.hatchRate, "clients/s...")

	random := rand.New(rand.NewSource(time.Now().Unix()))
	weightSum := r.getWeightSum()

	// The spawn count indicates the numbers of simulated "users" that should
	// be spawned. Each user then uses the provided tasks to perform a behavior.
	for i := 0; i < spawnCount; i++ {
		select {
		case <-quit:
			// The slave has been instructed to quit hatching.
			return
		default:
			// Spawn a user routine.
			go func() {
				for {
					select {
					case <-quit:
						// The user has been instructed to quit testing.
						return
					default:
						// Check rate limiter
						if r.rateLimitEnabled {
							if blocked := r.rateLimiter.Acquire(); blocked {
								continue
							}
						}

						var selected *Task
						if weightSum == 0 {
							// Roll a random task because no task weights were
							// provided to balance.
							index := random.Int63n(int64(len(r.tasks)))
							selected = r.tasks[index]
						} else {
							// Roll a random chance for a task to be performed.
							index := random.Float64()
							// fmt.Printf("Index is %f\n", index)

							// Get the selected task by user behavior.
							for _, task := range r.tasks {
								percent := float64(task.Weight) / float64(weightSum)
								// fmt.Printf("Percentage for \"%s\" is %f\n", task.Name, percent)
								if index <= percent {
									selected = task
									break
								}
							}
						}

						// Perform the task.
						if selected != nil {
							// fmt.Printf("Selected task \"%s\"\n", selected.Name)
							r.safeRun(selected.Fn)
						}
					}
				}
			}()

			// Increment the number of running clients.
			atomic.AddInt32(&r.numClients, 1)

			// The hatch rate indicates how quickly each user should be spawned.
			// A hatch rate of 0 indicates it should hatch all users immediately.
			if r.hatchRate != 0 && r.hatchType == "smooth" {
				<-time.After(time.Second / time.Duration(r.hatchRate))
			}
		}
	}

	if hatchCompleteFunc != nil {
		hatchCompleteFunc()
	}
}

func (r *runner) startHatching(spawnCount int, hatchRate int, hatchCompleteFunc func()) {
	fmt.Printf("startHatching was called with spawn count %d and hatchRate %d\n", spawnCount, hatchRate)

	r.stats.clearStatsChan <- true
	r.stopChan = make(chan bool)

	r.hatchRate = hatchRate
	r.numClients = 0

	// outputs should be started before boomer starts
	r.outputOnStart()

	go r.spawnWorkers(spawnCount, r.stopChan, hatchCompleteFunc)
}

func (r *runner) stop() {
	// outputs should be stopped before boomer stops
	r.outputOnStop()

	// publish the boomer stop event
	// user's code can subscribe to this event and do thins like cleaning up
	Events.Publish("boomer:stop")

	// stop previous goroutines without blocking
	// those goroutines will exit when r.safeRun returns
	close(r.stopChan)

	if r.rateLimitEnabled {
		r.rateLimiter.Stop()
	}
}

type localRunner struct {
	runner

	hatchCount int
}

func newLocalRunner(tasks []*Task, rateLimiter RateLimiter, hatchCount int, hatchType string, hatchRate int) (r *localRunner) {
	r = &localRunner{}
	r.tasks = tasks
	r.hatchType = hatchType
	r.hatchRate = hatchRate
	r.hatchCount = hatchCount
	r.closeChan = make(chan bool)
	r.addOutput(NewConsoleOutput())

	if rateLimiter != nil {
		r.rateLimitEnabled = true
		r.rateLimiter = rateLimiter
	}

	r.stats = newRequestStats()
	return r
}

func (r *localRunner) run() {
	r.state = stateInit
	r.stats.start()

	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		for {
			select {
			case data := <-r.stats.messageToRunnerChan:
				data["user_count"] = r.numClients
				r.outputOnEevent(data)
			case <-r.closeChan:
				Events.Publish("boomer:quit")
				r.stop()
				wg.Done()
				return
			}
		}
	}()

	if r.rateLimitEnabled {
		r.rateLimiter.Start()
	}
	r.startHatching(r.hatchCount, r.hatchRate, nil)

	wg.Wait()
}

func (r *localRunner) close() {
	if r.stats != nil {
		r.stats.close()
	}
	close(r.closeChan)
}

// SlaveRunner connects to the master, spawns goroutines and collects stats.
type slaveRunner struct {
	runner

	nodeID     string
	masterHost string
	masterPort int
	client     client
}

func newSlaveRunner(masterHost string, masterPort int, tasks []*Task, rateLimiter RateLimiter, hatchType string) (r *slaveRunner) {
	r = &slaveRunner{}
	r.masterHost = masterHost
	r.masterPort = masterPort
	r.tasks = tasks
	r.hatchType = hatchType
	r.nodeID = getNodeID()
	r.closeChan = make(chan bool)

	if rateLimiter != nil {
		r.rateLimitEnabled = true
		r.rateLimiter = rateLimiter
	}

	r.stats = newRequestStats()
	return r
}

func (r *slaveRunner) hatchComplete() {
	data := make(map[string]interface{})
	data["count"] = r.numClients
	r.client.sendChannel() <- newMessage("hatch_complete", data, r.nodeID)
	r.state = stateRunning
}

func (r *slaveRunner) onQuiting() {
	if r.state != stateQuitting {
		r.client.sendChannel() <- newMessage("quit", nil, r.nodeID)
	}
}

func (r *slaveRunner) close() {
	if r.stats != nil {
		r.stats.close()
	}
	if r.client != nil {
		r.client.close()
	}
	close(r.closeChan)
}

func (r *slaveRunner) onHatchMessage(msg *message) {
	rate, _ := msg.Data["hatch_rate"]
	hatchRate := int(rate.(float64))
	// 	if hatchRate == 0 {
	// 		// A hatch rate of 0 here indicates that the hatching of the workers
	// 		// should be done immediately. This with a workaround for boomer
	// 		// having a different meaning for hatchRate.
	// 		hatchRate = 1
	// 	}

	clients, _ := msg.Data["num_clients"]
	workers := 0
	if _, ok := clients.(uint64); ok {
		workers = int(clients.(uint64))
	} else {
		workers = int(clients.(int64))
	}

	log.Printf("Recv hatch message from master, num_clients is %d, hatch_rate is %d\n",
		workers, hatchRate)

	log.Print("calling sendChannel 'hatching'")
	r.client.sendChannel() <- newMessage("hatching", nil, r.nodeID)

	log.Print("publishing 'boomer:hatch'")
	Events.Publish("boomer:hatch", workers, hatchRate)

	log.Print("starting rate limiter")
	if r.rateLimitEnabled {
		r.rateLimiter.Start()
	}

	log.Print("starting hatching")
	r.startHatching(workers, hatchRate, r.hatchComplete)
}

// Runner acts as a state machine.
func (r *slaveRunner) onMessage(msg *message) {
	fmt.Printf("Received message: %+v", msg)

	switch r.state {
	case stateInit:
		switch msg.Type {
		case "hatch":
			fmt.Println("Received a hatch message while init")
			r.state = stateHatching
			r.onHatchMessage(msg)
		case "quit":
			Events.Publish("boomer:quit")
		}
	case stateHatching:
		fallthrough
	case stateRunning:
		switch msg.Type {
		case "hatch":
			fmt.Println("Received a hatch message while hatching or running")
			r.state = stateHatching
			r.stop()
			r.onHatchMessage(msg)
		case "stop":
			r.stop()
			r.state = stateStopped
			log.Println("Recv stop message from master, all the goroutines are stopped")
			r.client.sendChannel() <- newMessage("client_stopped", nil, r.nodeID)
			r.client.sendChannel() <- newMessage("client_ready", nil, r.nodeID)
			r.state = stateInit
		case "quit":
			r.stop()
			log.Println("Recv quit message from master, all the goroutines are stopped")
			Events.Publish("boomer:quit")
			r.state = stateInit
		}
	case stateStopped:
		switch msg.Type {
		case "hatch":
			fmt.Println("Received a hatch message while stopped")
			r.state = stateHatching
			r.onHatchMessage(msg)
		case "quit":
			Events.Publish("boomer:quit")
			r.state = stateInit
		}
	}
}

func (r *slaveRunner) startListener() {
	go func() {
		for {
			select {
			case msg := <-r.client.recvChannel():
				r.onMessage(msg)
			case <-r.closeChan:
				return
			}
		}
	}()
}

func (r *slaveRunner) run() {
	r.state = stateInit
	r.client = newClient(r.masterHost, r.masterPort, r.nodeID)

	err := r.client.connect()
	if err != nil {
		if strings.Contains(err.Error(), "Socket type DEALER is not compatible with PULL") {
			log.Println("Newer version of locust changes ZMQ socket to DEALER and ROUTER, you should update your locust version.")
		} else {
			log.Printf("Failed to connect to master(%s:%d) with error %v\n", r.masterHost, r.masterPort, err)
		}
		return
	}

	// listen to master
	r.startListener()

	r.stats.start()

	// tell master, I'm ready
	r.client.sendChannel() <- newMessage("client_ready", nil, r.nodeID)

	// report to master
	go func() {
		for {
			select {
			case data := <-r.stats.messageToRunnerChan:
				if r.state == stateInit || r.state == stateStopped {
					continue
				}
				data["user_count"] = r.numClients
				r.client.sendChannel() <- newMessage("stats", data, r.nodeID)
				r.outputOnEevent(data)
			case <-r.closeChan:
				return
			}
		}
	}()

	// heartbeat
	// See: https://github.com/locustio/locust/commit/a8c0d7d8c588f3980303358298870f2ea394ab93
	go func() {
		var ticker = time.NewTicker(heartbeatInterval)
		for {
			select {
			case <-ticker.C:
				data := map[string]interface{}{
					"state": r.state,
				}
				r.client.sendChannel() <- newMessage("heartbeat", data, r.nodeID)
			case <-r.closeChan:
				return
			}
		}
	}()

	Events.Subscribe("boomer:quit", r.onQuiting)
}
