package capacity

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/shirou/gopsutil/disk"
	"massnet.org/mass/logging"
	"massnet.org/mass/massutil/ccache"
	"massnet.org/mass/massutil/service"
	"massnet.org/mass/poc"
	"massnet.org/mass/poc/engine"
	"massnet.org/mass/poc/pocutil"
	"massnet.org/mass/pocec"
)

const (
	plotterMaxChanSize = 1024
	proofCacheSize     = 3000
	maxPoolWorker      = 32
	allState           = engine.LastState + 1 // allState includes all valid states
)

type PoCWallet interface {
	GenerateNewPublicKey() (*pocec.PublicKey, uint32, error)
	GetPublicKeyOrdinal(*pocec.PublicKey) (uint32, bool)
	SignMessage(pubKey *pocec.PublicKey, hash []byte) (*pocec.Signature, error)
	Unlock(password []byte) error
	Lock()
	IsLocked() bool
}

type SpaceKeeper struct {
	*service.BaseService
	stateLock             sync.RWMutex
	wg                    sync.WaitGroup
	quit                  chan struct{}
	configuring           int32 // atomic
	configured            int32 // atomic
	allowGenerateNewSpace bool
	dbDirs                []string
	dbType                string
	wallet                PoCWallet
	workSpaceIndex        []*WorkSpaceMap
	workSpaceList         []*WorkSpace
	queue                 *plotterQueue
	newQueuedWorkSpaceCh  chan *queuedWorkSpace
	proofCache            *ccache.CCache
	workerPool            *WorkerPool
	generateInitialIndex  func() error
	fileWatcher           func()
}

func (sk *SpaceKeeper) OnStart() error {
	if sk.wallet.IsLocked() {
		logging.CPrint(logging.ERROR, "can not start spaceKeeper with locked poc wallet", logging.LogFormat{"err": ErrWalletIsLocked})
		return ErrWalletIsLocked
	}

	sk.quit = make(chan struct{})
	go sk.spacePlotter()
	go sk.fileWatcher()
	logging.CPrint(logging.INFO, "spaceKeeper started")
	return nil
}

func (sk *SpaceKeeper) OnStop() error {
	close(sk.quit)
	sk.wg.Wait()
	logging.CPrint(logging.INFO, "spaceKeeper stopped")
	return nil
}

func (sk *SpaceKeeper) Type() string {
	return sk.Name()
}

func (sk *SpaceKeeper) WorkSpaceIDs(flags engine.WorkSpaceStateFlags) ([]string, error) {
	sk.stateLock.RLock()
	defer sk.stateLock.RUnlock()

	if flags.Contains(engine.SFAll) {
		idList := sk.workSpaceList
		result := make([]string, len(idList))
		for i := range result {
			result[i] = idList[i].id.String()
		}
		return result, nil
	}

	items := getWsByFlags(sk.workSpaceList, flags)
	result := make([]string, 0, len(items))
	for _, ws := range items {
		result = append(result, ws.id.String())
	}
	return result, nil
}

func (sk *SpaceKeeper) WorkSpaceInfos(flags engine.WorkSpaceStateFlags) ([]engine.WorkSpaceInfo, error) {
	sk.stateLock.RLock()
	defer sk.stateLock.RUnlock()

	if flags.Contains(engine.SFAll) {
		wsList := sk.workSpaceList
		result := make([]engine.WorkSpaceInfo, len(wsList))
		for i := range result {
			result[i] = wsList[i].Info()
		}
		return result, nil
	}

	items := getWsByFlags(sk.workSpaceList, flags)
	result := make([]engine.WorkSpaceInfo, 0, len(items))
	for _, ws := range items {
		result = append(result, ws.Info())
	}
	return result, nil
}

func (sk *SpaceKeeper) GetProof(ctx context.Context, sid string, challenge pocutil.Hash) (*engine.WorkSpaceProof, error) {
	if !sk.Started() {
		return nil, ErrSpaceKeeperIsNotRunning
	}

	if ws, ok := sk.workSpaceIndex[allState].Items()[sid]; ok && ws.using {
		return sk.getProof(ws, challenge), nil
	}
	return nil, ErrWorkSpaceDoesNotExist
}

func (sk *SpaceKeeper) GetProofs(ctx context.Context, flags engine.WorkSpaceStateFlags, challenge pocutil.Hash) ([]*engine.WorkSpaceProof, error) {
	if !sk.Started() {
		return nil, ErrSpaceKeeperIsNotRunning
	}

	items := make(map[string]*WorkSpace)
	for _, ws := range getWsByFlags(sk.workSpaceList, flags) {
		items[ws.id.String()] = ws
	}

	proofs := sk.getProofs(items, challenge)
	result := make([]*engine.WorkSpaceProof, 0, len(proofs))
	for _, proof := range proofs {
		result = append(result, proof)
	}
	return result, nil
}

func (sk *SpaceKeeper) GetProofReader(ctx context.Context, sid string, challenge pocutil.Hash) (engine.ProofReader, error) {
	if !sk.Started() {
		return nil, ErrSpaceKeeperIsNotRunning
	}

	if ws, ok := sk.workSpaceIndex[allState].Items()[sid]; ok && ws.using {
		prw := engine.NewProofRW(ctx, 1)
		go func() {
			if err := prw.Write(sk.getProof(ws, challenge)); err != nil {
				logging.CPrint(logging.WARN, "fail to write WorkSpaceProof to ProofRW", logging.LogFormat{"err": err, "sid": sid, "challenge": challenge})
			}
			prw.Close()
		}()
		return prw, nil
	}
	return nil, ErrWorkSpaceDoesNotExist
}

func (sk *SpaceKeeper) GetProofsReader(ctx context.Context, flags engine.WorkSpaceStateFlags, challenge pocutil.Hash) (engine.ProofReader, error) {
	if !sk.Started() {
		return nil, ErrSpaceKeeperIsNotRunning
	}

	items := make(map[string]*WorkSpace)
	for _, ws := range getWsByFlags(sk.workSpaceList, flags) {
		items[ws.id.String()] = ws
	}
	prw := engine.NewProofRW(ctx, len(items))
	go func() {
		proofs := sk.getProofs(items, challenge)
		var err error
		for i, proof := range proofs {
			if err = prw.Write(proof); err != nil {
				logging.CPrint(logging.WARN, "fail to write WorkSpaceProofs to ProofRW", logging.LogFormat{
					"err":       err,
					"index":     i,
					"count":     len(proofs),
					"flags":     flags,
					"challenge": challenge,
				})
				break
			}
		}
		prw.Close()
	}()
	return prw, nil
}

func (sk *SpaceKeeper) ActOnWorkSpace(sid string, action engine.ActionType) (err error) {
	if !action.IsValid() {
		return engine.ErrInvalidAction
	}

	switch action {
	case engine.Plot:
		err = sk.PlotWS(sid)
	case engine.Mine:
		err = sk.MineWS(sid)
	case engine.Stop:
		err = sk.StopWS(sid)
	case engine.Remove:
		err = sk.RemoveWS(sid)
	case engine.Delete:
		err = sk.DeleteWS(sid)
	default:
		err = engine.ErrUnImplementedAction
	}

	return err
}

func (sk *SpaceKeeper) ActOnWorkSpaces(flags engine.WorkSpaceStateFlags, action engine.ActionType) (errs map[string]error, err error) {
	if !action.IsValid() {
		return nil, engine.ErrInvalidAction
	}

	switch action {
	case engine.Plot:
		errs = sk.PlotMultiWS(flags)
	case engine.Mine:
		errs = sk.MineMultiWS(flags)
	case engine.Stop:
		errs = sk.StopMultiWS(flags)
	case engine.Remove:
		errs = sk.RemoveMultiWS(flags)
	case engine.Delete:
		errs = sk.DeleteMultiWS(flags)
	default:
		return nil, engine.ErrUnImplementedAction
	}

	return errs, nil
}

func (sk *SpaceKeeper) SignHash(sid string, hash [32]byte) (*pocec.Signature, error) {
	if ws, ok := sk.workSpaceIndex[allState].Items()[sid]; ok {
		return sk.wallet.SignMessage(ws.id.PubKey(), hash[:])
	}
	return nil, ErrWorkSpaceDoesNotExist
}

// PlotWS should make workSpace state conversion happen like:
// registered -> plotting -> ready
// registered -> ready
// plotting -> ready
// ready -> ready
// mining -> mining
func (sk *SpaceKeeper) PlotWS(sid string) error {
	sk.stateLock.RLock()
	defer sk.stateLock.RUnlock()

	if ws, ok := sk.workSpaceIndex[allState].Get(sid); !ok || !ws.using {
		return ErrWorkSpaceDoesNotExist
	}

	// registered -> plotting -> ready
	// registered -> ready
	// TODO: check for existence in plotterQueue
	if ws, ok := sk.workSpaceIndex[engine.Registered].Get(sid); ok {
		sk.newQueuedWorkSpaceCh <- newQueuedWorkSpace(ws, false)
		return nil
	}

	// plotting -> ready
	if _, ok := sk.workSpaceIndex[engine.Plotting].Get(sid); ok {
		// known that there's no more than one plotting workSpace at the same time
		qws := sk.queue.PoppedItem()
		if qws.ws.id.String() != sid {
			return ErrWorkSpaceIsNotPlotting
		}
		qws.wouldMining = false
		return nil
	}

	// ready -> ready
	// mining -> mining
	return nil
}

// MineWS should make workSpace state conversion happen like:
// registered -> plotting -> mining
// plotting   -> mining
// ready      -> mining
// mining     -> mining
// For registered workSpace, simply push it into spacePlotter Queue with `wouldMining = true`
// For plotting workSpace, modify queuedWorkspace with `wouldMining = true`
// For ready workSpace, convert it to mining state
func (sk *SpaceKeeper) MineWS(sid string) error {
	sk.stateLock.Lock()
	defer sk.stateLock.Unlock()

	if ws, ok := sk.workSpaceIndex[allState].Get(sid); !ok || !ws.using {
		return ErrWorkSpaceDoesNotExist
	}

	// registered -> plotting -> mining
	// TODO: check for existence in plotterQueue
	if ws, ok := sk.workSpaceIndex[engine.Registered].Get(sid); ok {
		sk.newQueuedWorkSpaceCh <- newQueuedWorkSpace(ws, true)
		return nil
	}

	// plotting -> mining
	if _, ok := sk.workSpaceIndex[engine.Plotting].Get(sid); ok {
		// known that there's no more than one plotting workSpace at the same time
		qws := sk.queue.PoppedItem()
		if qws.ws.id.String() != sid {
			return ErrWorkSpaceIsNotPlotting
		}
		qws.wouldMining = true
		return nil
	}

	// ready -> mining
	if ws, ok := sk.workSpaceIndex[engine.Ready].Get(sid); ok {
		sk.workSpaceIndex[engine.Ready].Delete(sid)
		sk.workSpaceIndex[engine.Mining].Set(sid, ws)
		ws.state = engine.Mining
		return nil
	}

	// mining -> mining
	return nil
}

// StopWs should make workSpace state conversion happen like:
// registered -> registered
// plotting   -> registered
// ready      -> ready
// mining     -> ready
// For all states, clear workSpace out from spacePlotter Queue
// For plotting workSpace, stop plotting and modify queuedWorkspace with `wouldMining = false`
// For mining workSpace, convert it to ready state
func (sk *SpaceKeeper) StopWS(sid string) error {
	sk.stateLock.Lock()
	defer sk.stateLock.Unlock()

	if ws, ok := sk.workSpaceIndex[allState].Get(sid); !ok || !ws.using {
		return ErrWorkSpaceDoesNotExist
	}

	sk.queue.Delete(sid)

	if ws, ok := sk.workSpaceIndex[engine.Plotting].Get(sid); ok {
		// known that there's no more than one plotting workSpace at the same time
		qws := sk.queue.PoppedItem()
		if qws.ws.id.String() != sid {
			return ErrWorkSpaceIsNotPlotting
		}
		qws.wouldMining = false
		return ws.StopPlot()
	}

	if ws, ok := sk.workSpaceIndex[engine.Mining].Get(sid); ok {
		sk.workSpaceIndex[engine.Mining].Delete(sid)
		sk.workSpaceIndex[engine.Ready].Set(sid, ws)
		ws.state = engine.Ready
		return nil
	}

	return nil
}

// RemoveWS should only be applied on registered/ready workSpace
// WorkSpace in spaceKeeper workSpaceList would be removed
func (sk *SpaceKeeper) RemoveWS(sid string) error {
	sk.stateLock.Lock()
	defer sk.stateLock.Unlock()

	var ok bool
	var ws *WorkSpace
	if ws, ok = sk.workSpaceIndex[allState].Get(sid); !ok || !ws.using {
		return ErrWorkSpaceDoesNotExist
	}

	sk.queue.Delete(sid)

	if ws, ok = sk.workSpaceIndex[engine.Registered].Get(sid); !ok {
		if ws, ok = sk.workSpaceIndex[engine.Ready].Get(sid); !ok {
			return ErrWorkSpaceIsNotStill
		}
	}

	sk.disuseWorkSpace(ws)
	return nil
}

// DeleteWS should only be applied on registered/ready workSpace
// WorkSpace in spaceKeeper index and data in MassDB would be both deleted
func (sk *SpaceKeeper) DeleteWS(sid string) error {
	sk.stateLock.Lock()
	defer sk.stateLock.Unlock()

	var ok bool
	var ws *WorkSpace
	if ws, ok = sk.workSpaceIndex[allState].Get(sid); !ok || !ws.using {
		return ErrWorkSpaceDoesNotExist
	}

	sk.queue.Delete(sid)

	if ws, ok = sk.workSpaceIndex[engine.Registered].Get(sid); !ok {
		if ws, ok = sk.workSpaceIndex[engine.Ready].Get(sid); !ok {
			return ErrWorkSpaceIsNotStill
		}
	}

	sk.workSpaceIndex[ws.state].Delete(sid)
	sk.workSpaceIndex[allState].Delete(sid)
	sk.disuseWorkSpace(ws)

	return ws.Delete()
}

func (sk *SpaceKeeper) PlotMultiWS(flags engine.WorkSpaceStateFlags) map[string]error {
	result := make(map[string]error)
	for _, ws := range getWsByFlags(sk.workSpaceList, flags) {
		sid := ws.id.String()
		result[sid] = sk.PlotWS(sid)
	}
	return result
}

func (sk *SpaceKeeper) MineMultiWS(flags engine.WorkSpaceStateFlags) map[string]error {
	result := make(map[string]error)
	for _, ws := range getWsByFlags(sk.workSpaceList, flags) {
		sid := ws.id.String()
		result[sid] = sk.MineWS(sid)
	}
	return result
}

func (sk *SpaceKeeper) StopMultiWS(flags engine.WorkSpaceStateFlags) map[string]error {
	result := make(map[string]error)
	for _, ws := range getWsByFlags(sk.workSpaceList, flags) {
		sid := ws.id.String()
		result[sid] = sk.StopWS(sid)
	}
	return result
}

func (sk *SpaceKeeper) RemoveMultiWS(flags engine.WorkSpaceStateFlags) map[string]error {
	result := make(map[string]error)
	for _, ws := range getWsByFlags(sk.workSpaceList, flags) {
		sid := ws.id.String()
		result[sid] = sk.RemoveWS(sid)
	}
	return result
}

func (sk *SpaceKeeper) DeleteMultiWS(flags engine.WorkSpaceStateFlags) map[string]error {
	result := make(map[string]error)
	for _, ws := range getWsByFlags(sk.workSpaceList, flags) {
		sid := ws.id.String()
		result[sid] = sk.DeleteWS(sid)
	}
	return result
}

func (sk *SpaceKeeper) Configured() bool {
	return atomic.LoadInt32(&sk.configured) != 0
}

func (sk *SpaceKeeper) ResetDBDirs(dbDirs []string) error {
	if sk.Started() {
		return ErrSpaceKeeperIsRunning
	}

	var strSliceEqual = func() bool {
		if len(dbDirs) != len(sk.dbDirs) {
			return false
		}
		existsDir := make(map[string]struct{})
		for _, dir := range sk.dbDirs {
			existsDir[dir] = struct{}{}
		}
		for _, dir := range dbDirs {
			if _, ok := existsDir[dir]; !ok {
				return false
			}
		}
		return true
	}

	if len(sk.dbDirs) == 0 {
		sk.dbDirs = dbDirs
		if err := sk.generateInitialIndex(); err != nil {
			return err
		}
		return nil
	}

	if !strSliceEqual() {
		return ErrSpaceKeeperChangeDBDirs
	}

	return nil

}

// TODO: consider more check items
func (sk *SpaceKeeper) checkOSDiskSize(requiredBytes int) error {
	if requiredBytes < 0 {
		return ErrInvalidRequiredBytes
	}
	info, err := disk.Usage(sk.dbDirs[0])
	if err != nil {
		return err
	}
	if uint64(requiredBytes) >= info.Free {
		return ErrOSDiskSizeNotEnough
	}
	return nil
}

func usableBitLength() []int {
	return []int{24, 26, 28}
}

// getIndexedWorkSpaces get all indexed workSpace grouped by bitLength
// slice of workSpace is sorted by same priority as in queuedWorkSpace
func (sk *SpaceKeeper) getIndexedWorkSpaces() map[int][]*WorkSpace {
	queueMap := make(map[int]*plotterQueue)
	for _, ws := range sk.workSpaceIndex[allState].Items() {
		bl := ws.id.bitLength
		qws := newQueuedWorkSpace(ws, false)
		if queue, exists := queueMap[bl]; exists {
			queue.Push(qws, qws.priority())
		} else {
			queueMap[bl] = newPlotterQueue()
			queueMap[bl].Push(qws, qws.priority())
		}
	}

	resultMap := make(map[int][]*WorkSpace)
	for bl, queue := range queueMap {
		resultMap[bl] = make([]*WorkSpace, queue.Size())
		for i := range resultMap[bl] {
			resultMap[bl][i] = queue.PopItem().ws
		}
	}
	return resultMap
}

func (sk *SpaceKeeper) getProof(ws *WorkSpace, challenge pocutil.Hash) *engine.WorkSpaceProof {
	var result *engine.WorkSpaceProof
	cacheKey := ws.id.String() + challenge.String()
	v, ok := sk.proofCache.Get(cacheKey)
	if ok {
		result = v.(*engine.WorkSpaceProof)
	} else {
		proof, err := ws.db.GetProof(challenge)
		result = &engine.WorkSpaceProof{
			SpaceID:   ws.id.String(),
			Proof:     proof,
			PublicKey: ws.id.PubKey(),
			Ordinal:   ws.id.Ordinal(),
			Error:     err,
		}
		sk.proofCache.Add(cacheKey, result)
	}
	return result
}

func (sk *SpaceKeeper) getProofs(wsMap map[string]*WorkSpace, challenge pocutil.Hash) map[string]*engine.WorkSpaceProof {
	logging.CPrint(logging.DEBUG, "generating thread pool jobs")
	result := make(map[string]*engine.WorkSpaceProof)
	jobList := make([]Job, 0, len(wsMap))

	var wg sync.WaitGroup
	for sid, ws := range wsMap {
		cacheKey := sid + challenge.String()
		v, ok := sk.proofCache.Get(cacheKey)
		if ok {
			result[sid] = v.(*engine.WorkSpaceProof)
		} else {
			job := Job{ws: ws, cacheKey: cacheKey, challenge: challenge, proofCache: sk.proofCache, result: sk.workerPool.result, wg: &wg}
			sk.workerPool.AddTask(job)
			jobList = append(jobList, job)
		}
	}
	wg.Add(1) // prevent len(wsMap) == 0
	wg.Done()
	wg.Wait()

	for _, job := range jobList {
		v, ok := sk.proofCache.Get(job.cacheKey)
		if ok {
			result[job.ws.id.String()] = v.(*engine.WorkSpaceProof)
		}
	}

	return result
}

func (sk *SpaceKeeper) useWorkSpace(ws *WorkSpace) {
	for _, e := range sk.workSpaceList {
		if e.id.String() == ws.id.String() {
			return
		}
	}
	sk.workSpaceList = append(sk.workSpaceList, ws)
	ws.using = true
}

func (sk *SpaceKeeper) disuseWorkSpace(ws *WorkSpace) {
	ws.using = false
	sk.workSpaceList = deleteFromSlice(sk.workSpaceList, ws.id.String())
}

// addWorkSpaceToIndex is not thread safe, should use lock in upper functions
func (sk *SpaceKeeper) addWorkSpaceToIndex(ws *WorkSpace) {
	sid := ws.id.String()
	if _, ok := sk.workSpaceIndex[allState].Get(sid); ok {
		return
	}

	sk.workSpaceIndex[allState].Set(sid, ws)
	sk.workSpaceIndex[ws.state].Set(sid, ws)
	return
}

// generateNewWorkSpace is not thread safe, should use lock in upper functions
func (sk *SpaceKeeper) generateNewWorkSpace(bitLength int) (*WorkSpace, error) {
	pubKey, ordinal, err := sk.wallet.GenerateNewPublicKey()
	if err != nil {
		return nil, err
	}

	return NewWorkSpace(sk.dbType, sk.dbDirs[0], int64(ordinal), pubKey, bitLength)
}

func (sk *SpaceKeeper) ConfigureByBitLength(BlCount map[int]int, execPlot, execMine bool) ([]engine.WorkSpaceInfo, error) {
	if sk.Started() {
		return nil, ErrSpaceKeeperIsRunning
	}
	if !atomic.CompareAndSwapInt32(&sk.configuring, 0, 1) {
		return nil, ErrSpaceKeeperIsConfiguring
	}
	defer atomic.StoreInt32(&sk.configuring, 0)
	atomic.StoreInt32(&sk.configured, 0)

	var failureReturn = func(err error) ([]engine.WorkSpaceInfo, error) {
		return nil, err
	}

	var finished bool
	var currentCount = make(map[int]int)
	var resultList = make([]*WorkSpace, 0)

	var successfullyReturn = func() ([]engine.WorkSpaceInfo, error) {
		if len(resultList) == 0 {
			logging.CPrint(logging.ERROR, "configured nothing by bitLength", logging.LogFormat{"target": BlCount, "result": resultList})
			return failureReturn(ErrSpaceKeeperConfiguredNothing)
		}
		// Push workSpaces into queue (for spacePlotter)
		for _, ws := range sk.workSpaceList {
			ws.using = false
		}
		sk.workSpaceList = make([]*WorkSpace, 0)
		tmpQueuedList := newPlotterQueue()
		for _, ws := range resultList {
			qws := newQueuedWorkSpace(ws, execMine)
			tmpQueuedList.Push(qws, qws.priority())
		}
		for !tmpQueuedList.Empty() {
			qws := tmpQueuedList.PopItem()
			sk.queue.Push(qws, qws.priority())
			sk.useWorkSpace(qws.ws)
		}
		if !(execMine || execPlot) {
			sk.queue.Reset()
		}
		wsiList := make([]engine.WorkSpaceInfo, len(resultList))
		for i, ws := range resultList {
			wsiList[i] = ws.Info()
		}

		atomic.StoreInt32(&sk.configured, 1)
		return wsiList, nil
	}

	// try to fill list by indexed spaces
	sk.workSpaceIndex[engine.Ready].Items()
	resultList, currentCount, finished = fillSpaceListByBitLength(resultList, sk.getIndexedWorkSpaces(), currentCount, BlCount)
	if finished {
		return successfullyReturn()
	}

	// try to generate new WorkSpace to fill list
	var err error
	resultList, err = sk.generateFillSpaceListByBitLength(resultList, currentCount, BlCount)
	if err != nil {
		return failureReturn(err)
	}

	return successfullyReturn()
}

// fillSpaceListByBitLength fills list by srcMap WorkSpaces of different BitLengths.
// It returns true if targetCount is satisfied.
func fillSpaceListByBitLength(dstList []*WorkSpace, srcMap map[int][]*WorkSpace, currentCount, targetCount map[int]int) ([]*WorkSpace, map[int]int, bool) {
	var blFinished bool
	var finished = true

	for bl, count := range targetCount {
		blFinished = false
		if _, exists := srcMap[bl]; !exists {
			finished = false
			continue
		}

		for _, space := range srcMap[bl] {
			if currentCount[bl] == count {
				blFinished = true
				break
			}
			dstList = append(dstList, space)
			currentCount[bl]++
		}
		blFinished = currentCount[bl] == count
		finished = finished && blFinished
	}

	return dstList, currentCount, finished
}

func (sk *SpaceKeeper) generateFillSpaceListByBitLength(dstList []*WorkSpace, currentCount, targetCount map[int]int) ([]*WorkSpace, error) {
	if !sk.allowGenerateNewSpace {
		return nil, ErrWorkSpaceCannotGenerate
	}
	// check os disk size
	var requiredOSDiskSize int
	for bl, target := range targetCount {
		requiredOSDiskSize += (target - currentCount[bl]) * poc.BitLengthDiskSize[bl]
	}
	if err := sk.checkOSDiskSize(requiredOSDiskSize); err != nil {
		return nil, err
	}

	// generate new WorkSpaces of different BitLengths, until targetCount is satisfied
	for bl, count := range targetCount {
	out:
		for {
			if currentCount[bl] == count {
				break out
			}
			newWS, err := sk.generateNewWorkSpace(bl)
			if err != nil {
				return nil, err
			}
			sk.addWorkSpaceToIndex(newWS)
			dstList = append(dstList, newWS)
			currentCount[bl]++
		}
	}

	return dstList, nil
}

func (sk *SpaceKeeper) ConfigureBySize(targetSize int, password string) ([]engine.WorkSpaceInfo, error) {
	if sk.Started() {
		return nil, ErrSpaceKeeperIsRunning
	}
	if !atomic.CompareAndSwapInt32(&sk.configuring, 0, 1) {
		return nil, ErrSpaceKeeperIsConfiguring
	}
	defer atomic.StoreInt32(&sk.configuring, 0)
	atomic.StoreInt32(&sk.configured, 0)

	var failureReturn = func(err error) ([]engine.WorkSpaceInfo, error) {
		return nil, err
	}

	if targetSize < poc.BitLengthDiskSize[usableBitLength()[0]] {
		return failureReturn(ErrConfigUnderSizeTarget)
	}

	if err := sk.wallet.Unlock([]byte(password)); err != nil {
		return nil, err
	}

	var currentSize = 0
	var finished bool
	var resultList = make([]*WorkSpace, 0)

	var successfullyReturn = func() ([]engine.WorkSpaceInfo, error) {
		if len(resultList) == 0 {
			logging.CPrint(logging.ERROR, "configured nothing by size", logging.LogFormat{"target": targetSize, "result": resultList})
			return failureReturn(ErrSpaceKeeperConfiguredNothing)
		}

		// Push workSpaces into queue (for spacePlotter)
		sk.queue.Reset()
		for _, ws := range sk.workSpaceList {
			ws.using = false
		}
		sk.workSpaceList = make([]*WorkSpace, 0)
		tmpQueuedList := newPlotterQueue()
		for _, ws := range resultList {
			qws := newQueuedWorkSpace(ws, false)
			tmpQueuedList.Push(qws, qws.priority())
		}
		for !tmpQueuedList.Empty() {
			qws := tmpQueuedList.PopItem()
			sk.queue.Push(qws, qws.priority())
			sk.useWorkSpace(qws.ws)
		}
		sk.queue.Reset()
		wsiList := make([]engine.WorkSpaceInfo, len(resultList))
		for i, ws := range resultList {
			wsiList[i] = ws.Info()
		}

		atomic.StoreInt32(&sk.configured, 1)
		return wsiList, nil
	}

	// try to fill list by indexed spaces
	resultList, currentSize, finished = fillSpaceListBySize(resultList, sk.getIndexedWorkSpaces(), currentSize, targetSize)
	if finished {
		return successfullyReturn()
	}

	// try to generate new WorkSpace to fill list
	var err error
	resultList, _, err = sk.generateFillSpaceListBySize(resultList, currentSize, targetSize)
	if err != nil {
		return failureReturn(err)
	}

	return successfullyReturn()
}

func fillSpaceListBySize(dstList []*WorkSpace, srcMap map[int][]*WorkSpace, currentSize, targetSize int) ([]*WorkSpace, int, bool) {
	// get allowed BitLength in decreasing order
	allowedBL := usableBitLength()
	tmpLen := len(allowedBL)
	for i := 0; i < tmpLen/2; i++ {
		allowedBL[i], allowedBL[tmpLen-1-i] = allowedBL[tmpLen-1-i], allowedBL[i]
	}
	// fill list by WorkSpaces from srcMap, until targetSize is satisfied
	for _, bl := range allowedBL {
		if _, exists := srcMap[bl]; !exists {
			continue
		}
		for _, space := range srcMap[bl] {
			currentSize += poc.BitLengthDiskSize[bl]
			if currentSize > targetSize {
				currentSize -= poc.BitLengthDiskSize[bl]
				continue
			}
			dstList = append(dstList, space)
		}
	}

	// returns true if target size is satisfied
	if currentSize == targetSize || targetSize-currentSize < poc.MinDiskSize {
		return dstList, currentSize, true
	}

	// returns false if target size is not satisfied
	return dstList, currentSize, false
}

func (sk *SpaceKeeper) generateFillSpaceListBySize(dstList []*WorkSpace, currentSize, targetSize int) ([]*WorkSpace, int, error) {
	if !sk.allowGenerateNewSpace {
		return nil, currentSize, ErrWorkSpaceCannotGenerate
	}
	// check os disk size
	if err := sk.checkOSDiskSize(targetSize - currentSize); err != nil {
		return nil, currentSize, err
	}
	// get allowed BitLength in decreasing order
	allowedBL := usableBitLength()
	tmpLen := len(allowedBL)
	for i := 0; i < tmpLen/2; i++ {
		allowedBL[i], allowedBL[tmpLen-1-i] = allowedBL[tmpLen-1-i], allowedBL[i]
	}
	// generate new WorkSpaces of different BitLengths, until targetSize is satisfied
	for _, bl := range allowedBL {
	out:
		for {
			if targetSize-currentSize < poc.BitLengthDiskSize[bl] {
				// Current BitLength is too large
				break out
			}
			currentSize += poc.BitLengthDiskSize[bl]
			newWS, err := sk.generateNewWorkSpace(bl)
			if err != nil {
				return nil, currentSize, err
			}
			sk.addWorkSpaceToIndex(newWS)
			dstList = append(dstList, newWS)
		}
	}

	return dstList, currentSize, nil
}

func (sk *SpaceKeeper) ConfigureByPubKey(PubKeyBL map[*pocec.PublicKey]int, PubKeyOrdinal map[*pocec.PublicKey]int, execPlot, execMine bool) ([]engine.WorkSpaceInfo, error) {
	if sk.Started() {
		return nil, ErrSpaceKeeperIsRunning
	}
	if !atomic.CompareAndSwapInt32(&sk.configuring, 0, 1) {
		return nil, ErrSpaceKeeperIsConfiguring
	}
	defer atomic.StoreInt32(&sk.configuring, 0)
	atomic.StoreInt32(&sk.configured, 0)

	var failureReturn = func(err error) ([]engine.WorkSpaceInfo, error) {
		return nil, err
	}
	var resultList = make([]*WorkSpace, 0)
	var successfullyReturn = func() ([]engine.WorkSpaceInfo, error) {
		if len(resultList) != len(PubKeyBL) {
			logging.CPrint(logging.ERROR, "configured not enough by pubKey", logging.LogFormat{"target": PubKeyBL, "result": resultList})
			return failureReturn(ErrSpaceKeeperConfiguredNothing)
		}
		// Push workSpaces into queue (for spacePlotter)
		sk.workSpaceList = make([]*WorkSpace, 0)
		tmpQueuedList := newPlotterQueue()
		for _, ws := range resultList {
			qws := newQueuedWorkSpace(ws, execMine)
			tmpQueuedList.Push(qws, qws.priority())
		}
		for !tmpQueuedList.Empty() {
			qws := tmpQueuedList.PopItem()
			sk.queue.Push(qws, qws.priority())
			sk.useWorkSpace(qws.ws)
		}
		if !(execMine || execPlot) {
			sk.queue.Reset()
		}
		wsiList := make([]engine.WorkSpaceInfo, len(resultList))
		for i, ws := range resultList {
			wsiList[i] = ws.Info()
		}

		atomic.StoreInt32(&sk.configured, 1)
		return wsiList, nil
	}

	resultList, err := sk.generateFillSpaceListByPubKey(resultList, PubKeyBL, PubKeyOrdinal)
	if err != nil {
		return failureReturn(err)
	}

	return successfullyReturn()
}

func (sk *SpaceKeeper) generateFillSpaceListByPubKey(dstList []*WorkSpace, targetPubKeyBL map[*pocec.PublicKey]int, pubKeyOrdinal map[*pocec.PublicKey]int) ([]*WorkSpace, error) {
	// check OS disk size
	var requiredOSDiskSize int
	for pubKey, bl := range targetPubKeyBL {
		if _, exists := sk.workSpaceIndex[allState].Get(NewSpaceID(int64(pubKeyOrdinal[pubKey]), pubKey, bl).String()); !exists {
			requiredOSDiskSize += poc.BitLengthDiskSize[bl]
		}
	}
	if err := sk.checkOSDiskSize(requiredOSDiskSize); err != nil {
		return nil, err
	}

	for pubKey, bl := range targetPubKeyBL {
		if ws, exists := sk.workSpaceIndex[allState].Get(NewSpaceID(int64(pubKeyOrdinal[pubKey]), pubKey, bl).String()); exists {
			dstList = append(dstList, ws)
			continue
		}
		newWS, err := sk.generateNewWorkSpaceByPubKey(int64(pubKeyOrdinal[pubKey]), pubKey, bl)
		if err != nil {
			return nil, err
		}
		sk.addWorkSpaceToIndex(newWS)
		dstList = append(dstList, newWS)
	}

	return dstList, nil
}

// generateNewWorkSpace is not thread safe, should use lock in upper functions
func (sk *SpaceKeeper) generateNewWorkSpaceByPubKey(ordinal int64, pubKey *pocec.PublicKey, bitLength int) (*WorkSpace, error) {
	return NewWorkSpace(sk.dbType, sk.dbDirs[0], ordinal, pubKey, bitLength)
}

func (sk *SpaceKeeper) ConfigureByFlags(flags engine.WorkSpaceStateFlags, execPlot, execMine bool) ([]engine.WorkSpaceInfo, error) {
	if sk.Started() {
		return nil, ErrSpaceKeeperIsRunning
	}
	if !atomic.CompareAndSwapInt32(&sk.configuring, 0, 1) {
		return nil, ErrSpaceKeeperIsConfiguring
	}
	defer atomic.StoreInt32(&sk.configuring, 0)
	atomic.StoreInt32(&sk.configured, 0)

	items := make([]*WorkSpace, 0)
	for _, state := range flags.States() {
		m := sk.workSpaceIndex[state].Items()
		for _, ws := range m {
			items = append(items, ws)
		}
	}

	sk.workSpaceList = make([]*WorkSpace, 0)
	tmpQueuedList := newPlotterQueue()
	for _, ws := range items {
		qws := newQueuedWorkSpace(ws, true)
		tmpQueuedList.Push(qws, qws.priority())
	}
	for !tmpQueuedList.Empty() {
		qws := tmpQueuedList.PopItem()
		sk.queue.Push(qws, qws.priority())
		sk.useWorkSpace(qws.ws)
	}
	if !(execMine || execPlot) {
		sk.queue.Reset()
	}
	wsiList := make([]engine.WorkSpaceInfo, len(sk.workSpaceList))
	for i, ws := range sk.workSpaceList {
		wsiList[i] = ws.Info()
	}

	if len(wsiList) == 0 {
		atomic.StoreInt32(&sk.configured, 1)
	}

	return wsiList, nil
}

func deleteFromSlice(src []*WorkSpace, sid string) []*WorkSpace {
	if len(src) == 0 {
		return src
	}

	var idx int
	var exists bool
	for i, ws := range src {
		if ws.id.String() == sid {
			idx, exists = i, true
			break
		}
	}

	if !exists {
		return src
	}

	result := make([]*WorkSpace, len(src)-1)
	copy(result, src[:idx])
	copy(result[idx:], src[idx+1:])
	return result
}

func getWsByID(src []*WorkSpace, sid string) (*WorkSpace, bool) {
	for _, ws := range src {
		if ws.id.String() == sid {
			return ws, true
		}
	}
	return nil, false
}

func getWsByFlags(src []*WorkSpace, flags engine.WorkSpaceStateFlags) []*WorkSpace {
	states := make(map[engine.WorkSpaceState]bool)
	for _, state := range flags.States() {
		states[state] = true
	}
	result := make([]*WorkSpace, 0, len(src))
	for _, ws := range src {
		if states[ws.state] {
			result = append(result, ws)
		}
	}
	return result
}
