package gpuminer

import (
	"bytes"
	"encoding/binary"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	stratum "github.com/gurupras/go-stratum-client"
	cpuminer "github.com/gurupras/go-stratum-client/cpu-miner"
	"github.com/gurupras/go-stratum-client/cpu-miner/xmrig_crypto"
	amdgpu "github.com/gurupras/go-stratum-client/gpu-miner/amd"
	"github.com/gurupras/go-stratum-client/miner"
	"github.com/rainliu/gocl/cl"
	log "github.com/sirupsen/logrus"
)

var (
	TotalMiners uint32 = 0
	minerId     uint32 = 0
)

type GPUMiner struct {
	*stratum.StratumContext
	*miner.Miner
	Context   *amdgpu.GPUContext
	Index     int
	Intensity int
	WorkSize  int
	debug     bool
}

func NewGPUMiner(sc *stratum.StratumContext, index, intensity, worksize int) *GPUMiner {
	miner := &GPUMiner{
		sc,
		miner.New(minerId),
		amdgpu.New(index, intensity, worksize),
		index,
		intensity,
		worksize,
		false,
	}
	atomic.AddUint32(&TotalMiners, 1)
	atomic.AddUint32(&minerId, 1)
	return miner
}

func (m *GPUMiner) SetDebug(val bool) {
	m.debug = val
}

type CLResult []cl.CL_int

func (clr CLResult) Bytes() []byte {
	var dummy cl.CL_int
	ret := make([]byte, len(clr)*int(unsafe.Sizeof(dummy)))
	buf := bytes.NewBuffer(ret)

	b := make([]byte, 4)
	for _, v := range clr {
		binary.LittleEndian.PutUint32(b, uint32(v))
		buf.Write(b)
	}
	return ret
}

func (clr CLResult) Zero() {
	for i := 0; i < len(clr); i++ {
		clr[i] = 0
	}
}

func (m *GPUMiner) Run() error {
	results := make(CLResult, 0x100)

	defaultNonce := 0xffffffff / int(TotalMiners) * (int(m.Id()))
	workLock := sync.Mutex{}
	work := xmrig_crypto.NewXMRigWork()
	var newWork *stratum.Work
	noncePtr := work.NoncePtr

	workChan := make(chan *stratum.Work, 0)

	initialWg := sync.WaitGroup{}
	initialWg.Add(1)
	gotFirstJob := false

	m.StratumContext.RegisterWorkListener(workChan)

	// Call with workLock acquired
	consumeWork := func() {
		if newWork == nil || strings.Compare(newWork.JobID, work.JobID) == 0 {
			return
		}
		//log.Debugf("Thread-%d: Got new work - %s", m.id, newWork.JobID)
		//log.Debugf("Thread-%d: blob: %v", stratum.BinToStr(newWork.Data))
		cpuminer.WorkCopy(work.Work, newWork)
		work.UpdateCData()
		*noncePtr = uint32(defaultNonce)
		amdgpu.XMRSetWork(m.Context, work.Data, work.Size, work.Target)
	}

	go func() {
		for work := range workChan {
			workLock.Lock()
			newWork = work
			consumeWork()
			if !gotFirstJob {
				gotFirstJob = true
				initialWg.Done()
			}
			workLock.Unlock()
			log.Debugf("miner-new-work: Updated work - %s", newWork.JobID)
			log.Debugf("miner-new-work: target=%X", newWork.Target)
		}
	}()

	initialWg.Wait()
	log.Debugf("Got first job")

	callCount := 0
	callCountTime := time.Now()
	var (
		runWorkDuration int64
		tempTime        time.Time
	)

	// Main loop
	for {
		curWork := work
		results.Zero()

		if m.debug {
			tempTime = time.Now()
			amdgpu.XMRRunWork(m.Context, results)
			// amdgpu.RunWork(m.Context, results)
			runWorkDuration += time.Now().Sub(tempTime).Nanoseconds()
			callCount++
		} else {
			amdgpu.XMRRunWork(m.Context, results)
		}

		for i := 0; i < int(results[0xFF]); i++ {
			*noncePtr = uint32(results[i])
			m.SubmitWork(curWork)
		}

		now := time.Now()
		if m.debug && now.Sub(callCountTime) > 10*time.Second {
			log.Infof("calls=%d", callCount)
			log.Infof("s/iter XMRunWork=%.2f", time.Duration(runWorkDuration/int64(callCount)).Seconds())
			runWorkDuration = 0
			callCount = 0
			callCountTime = now
		}
		m.InformHashrate(uint32(m.Context.RawIntensity))
	}
}

// We need to check the hash. So just send the work down on HashCheckChan
func (m *GPUMiner) SubmitWork(work *xmrig_crypto.XMRigWork) error {
	hashResult := &HashResult{
		m.Id(),
		m.StratumContext,
		work,
	}
	HashCheckChan <- hashResult
	return nil
}