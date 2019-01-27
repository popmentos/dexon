// Copyright 2018 The dexon-consensus Authors
// This file is part of the dexon-consensus library.
//
// The dexon-consensus library is free software: you can redistribute it
// and/or modify it under the terms of the GNU Lesser General Public License as
// published by the Free Software Foundation, either version 3 of the License,
// or (at your option) any later version.
//
// The dexon-consensus library is distributed in the hope that it will be
// useful, but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the GNU Lesser
// General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the dexon-consensus library. If not, see
// <http://www.gnu.org/licenses/>.

package core

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dexon-foundation/dexon-consensus/common"
	"github.com/dexon-foundation/dexon-consensus/core/types"
	"github.com/dexon-foundation/dexon-consensus/core/utils"
)

// Errors returned from BA modules
var (
	ErrPreviousRoundIsNotFinished = errors.New("previous round is not finished")
)

const maxResultCache = 100

// genValidLeader generate a validLeader function for agreement modules.
func genValidLeader(
	mgr *agreementMgr) func(*types.Block) (bool, error) {
	return func(block *types.Block) (bool, error) {
		if block.Timestamp.After(time.Now()) {
			return false, nil
		}
		if err := mgr.lattice.SanityCheck(block, true); err != nil {
			if err == ErrRetrySanityCheckLater {
				return false, nil
			}
			return false, err
		}
		mgr.logger.Debug("Calling Application.VerifyBlock", "block", block)
		switch mgr.app.VerifyBlock(block) {
		case types.VerifyInvalidBlock:
			return false, ErrInvalidBlock
		case types.VerifyRetryLater:
			return false, nil
		default:
		}
		return true, nil
	}
}

type agreementMgrConfig struct {
	beginTime     time.Time
	numChains     uint32
	roundInterval time.Duration
	notarySetSize uint32
	lambdaBA      time.Duration
	crs           common.Hash
}

type baRoundSetting struct {
	chainID   uint32
	notarySet map[types.NodeID]struct{}
	agr       *agreement
	recv      *consensusBAReceiver
	ticker    Ticker
	crs       common.Hash
}

type agreementMgr struct {
	// TODO(mission): unbound Consensus instance from this module.
	con               *Consensus
	ID                types.NodeID
	app               Application
	gov               Governance
	network           Network
	logger            common.Logger
	cache             *utils.NodeSetCache
	signer            *utils.Signer
	lattice           *Lattice
	ctx               context.Context
	lastEndTime       time.Time
	initRound         uint64
	configs           []*agreementMgrConfig
	baModules         []*agreement
	processedBAResult map[types.Position]struct{}
	voteFilters       []*utils.VoteFilter
	waitGroup         sync.WaitGroup
	pendingVotes      map[uint64][]*types.Vote
	pendingBlocks     map[uint64][]*types.Block
	isRunning         bool

	// This lock should be used when attempting to:
	//  - add a new baModule.
	//  - remove all baModules when stopping. In this case, the cleaner need
	//    to wait for all routines runnning baModules finished.
	//  - access a method of baModule.
	//  - append a config from new round.
	// The routine running corresponding baModule, however, doesn't have to
	// acquire this lock.
	lock sync.RWMutex
}

func newAgreementMgr(con *Consensus, initRound uint64,
	initRoundBeginTime time.Time) *agreementMgr {
	return &agreementMgr{
		con:               con,
		ID:                con.ID,
		app:               con.app,
		gov:               con.gov,
		network:           con.network,
		logger:            con.logger,
		cache:             con.nodeSetCache,
		signer:            con.signer,
		lattice:           con.lattice,
		ctx:               con.ctx,
		initRound:         initRound,
		lastEndTime:       initRoundBeginTime,
		processedBAResult: make(map[types.Position]struct{}, maxResultCache),
	}
}

func (mgr *agreementMgr) getConfig(round uint64) *agreementMgrConfig {
	mgr.lock.RLock()
	defer mgr.lock.RUnlock()
	if round < mgr.initRound {
		panic(ErrRoundOutOfRange)
	}
	roundIndex := round - mgr.initRound
	if roundIndex >= uint64(len(mgr.configs)) {
		return nil
	}
	return mgr.configs[roundIndex]
}

func (mgr *agreementMgr) run() {
	mgr.lock.Lock()
	defer mgr.lock.Unlock()
	if mgr.isRunning {
		return
	}
	mgr.isRunning = true
	for i := uint32(0); i < uint32(len(mgr.baModules)); i++ {
		mgr.waitGroup.Add(1)
		go func(idx uint32) {
			defer mgr.waitGroup.Done()
			mgr.runBA(mgr.initRound, idx)
		}(i)
	}
}

func (mgr *agreementMgr) appendConfig(
	round uint64, config *types.Config, crs common.Hash) (err error) {
	mgr.lock.Lock()
	defer mgr.lock.Unlock()
	if round != uint64(len(mgr.configs))+mgr.initRound {
		return ErrRoundNotIncreasing
	}
	newConfig := &agreementMgrConfig{
		beginTime:     mgr.lastEndTime,
		numChains:     config.NumChains,
		roundInterval: config.RoundInterval,
		notarySetSize: config.NotarySetSize,
		lambdaBA:      config.LambdaBA,
		crs:           crs,
	}
	mgr.configs = append(mgr.configs, newConfig)
	mgr.lastEndTime = mgr.lastEndTime.Add(config.RoundInterval)
	// Create baModule for newly added chain.
	for i := uint32(len(mgr.baModules)); i < newConfig.numChains; i++ {
		// Prepare modules.
		recv := &consensusBAReceiver{
			consensus:     mgr.con,
			chainID:       i,
			restartNotary: make(chan types.Position, 1),
			roundValue:    &atomic.Value{},
		}
		recv.roundValue.Store(uint64(0))
		agrModule := newAgreement(
			mgr.con.ID,
			recv,
			newLeaderSelector(genValidLeader(mgr), mgr.logger),
			mgr.signer,
			mgr.logger)
		// Hacky way to initialize first notarySet.
		nodes, err := mgr.cache.GetNodeSet(round)
		if err != nil {
			return err
		}
		agrModule.notarySet = nodes.GetSubSet(
			int(config.NotarySetSize),
			types.NewNotarySetTarget(crs, i))
		// Hacky way to make agreement module self contained.
		recv.agreementModule = agrModule
		mgr.baModules = append(mgr.baModules, agrModule)
		mgr.voteFilters = append(mgr.voteFilters, utils.NewVoteFilter())
		if mgr.isRunning {
			mgr.waitGroup.Add(1)
			go func(idx uint32) {
				defer mgr.waitGroup.Done()
				mgr.runBA(round, idx)
			}(i)
		}
	}
	return nil
}

func (mgr *agreementMgr) processVote(v *types.Vote) error {
	mgr.lock.RLock()
	defer mgr.lock.RUnlock()
	if v.Position.ChainID >= uint32(len(mgr.baModules)) {
		mgr.logger.Error("Process vote for unknown chain to BA",
			"position", &v.Position,
			"baChain", len(mgr.baModules),
			"baRound", len(mgr.configs),
			"initRound", mgr.initRound)
		return utils.ErrInvalidChainID
	}
	filter := mgr.voteFilters[v.Position.ChainID]
	if filter.Filter(v) {
		return nil
	}
	v = v.Clone()
	err := mgr.baModules[v.Position.ChainID].processVote(v)
	if err == nil {
		mgr.baModules[v.Position.ChainID].updateFilter(filter)
	}
	return err
}

func (mgr *agreementMgr) processBlock(b *types.Block) error {
	mgr.lock.RLock()
	defer mgr.lock.RUnlock()
	if b.Position.ChainID >= uint32(len(mgr.baModules)) {
		mgr.logger.Error("Process block for unknown chain to BA",
			"position", &b.Position,
			"baChain", len(mgr.baModules),
			"baRound", len(mgr.configs),
			"initRound", mgr.initRound)
		return utils.ErrInvalidChainID
	}
	return mgr.baModules[b.Position.ChainID].processBlock(b)
}

func (mgr *agreementMgr) touchAgreementResult(
	result *types.AgreementResult) (first bool) {
	// DO NOT LOCK THIS FUNCTION!!!!!!!! YOU WILL REGRET IT!!!!!
	if _, exist := mgr.processedBAResult[result.Position]; !exist {
		first = true
		if len(mgr.processedBAResult) > maxResultCache {
			for k := range mgr.processedBAResult {
				// Randomly drop one element.
				delete(mgr.processedBAResult, k)
				break
			}
		}
		mgr.processedBAResult[result.Position] = struct{}{}
	}
	return
}

func (mgr *agreementMgr) untouchAgreementResult(
	result *types.AgreementResult) {
	// DO NOT LOCK THIS FUNCTION!!!!!!!! YOU WILL REGRET IT!!!!!
	delete(mgr.processedBAResult, result.Position)
}

func (mgr *agreementMgr) processAgreementResult(
	result *types.AgreementResult) error {
	mgr.lock.RLock()
	defer mgr.lock.RUnlock()
	if result.Position.ChainID >= uint32(len(mgr.baModules)) {
		mgr.logger.Error("Process unknown result for unknown chain to BA",
			"position", &result.Position,
			"baChain", len(mgr.baModules),
			"baRound", len(mgr.configs),
			"initRound", mgr.initRound)
		return utils.ErrInvalidChainID
	}
	agreement := mgr.baModules[result.Position.ChainID]
	aID := agreement.agreementID()
	if isStop(aID) {
		return nil
	}
	if result.Position == aID && !agreement.confirmed() {
		mgr.logger.Info("Syncing BA", "position", &result.Position)
		for key := range result.Votes {
			if err := agreement.processVote(&result.Votes[key]); err != nil {
				return err
			}
		}
	} else if result.Position.Newer(&aID) {
		mgr.logger.Info("Fast syncing BA", "position", &result.Position)
		nodes, err := mgr.cache.GetNodeSet(result.Position.Round)
		if err != nil {
			return err
		}
		mgr.logger.Debug("Calling Network.PullBlocks for fast syncing BA",
			"hash", result.BlockHash)
		mgr.network.PullBlocks(common.Hashes{result.BlockHash})
		mgr.logger.Debug("Calling Governance.CRS", "round", result.Position.Round)
		crs := utils.GetCRSWithPanic(mgr.gov, result.Position.Round, mgr.logger)
		nIDs := nodes.GetSubSet(
			int(utils.GetConfigWithPanic(
				mgr.gov, result.Position.Round, mgr.logger).NotarySetSize),
			types.NewNotarySetTarget(crs, result.Position.ChainID))
		for key := range result.Votes {
			if err := agreement.processVote(&result.Votes[key]); err != nil {
				return err
			}
		}
		leader, err := mgr.cache.GetLeaderNode(result.Position)
		if err != nil {
			return err
		}
		agreement.restart(nIDs, result.Position, leader, crs)
	}
	return nil
}

func (mgr *agreementMgr) stop() {
	// Stop all running agreement modules.
	func() {
		mgr.lock.Lock()
		defer mgr.lock.Unlock()
		for _, agr := range mgr.baModules {
			agr.stop()
		}
	}()
	// Block until all routines are done.
	mgr.waitGroup.Wait()
}

func (mgr *agreementMgr) runBA(initRound uint64, chainID uint32) {
	// Acquire agreement module.
	agr, recv := func() (*agreement, *consensusBAReceiver) {
		mgr.lock.RLock()
		defer mgr.lock.RUnlock()
		agr := mgr.baModules[chainID]
		return agr, agr.data.recv.(*consensusBAReceiver)
	}()
	// These are round based variables.
	var (
		currentRound uint64
		nextRound    = initRound
		setting      = baRoundSetting{
			chainID: chainID,
			agr:     agr,
			recv:    recv,
		}
		roundBeginTime time.Time
		roundEndTime   time.Time
		tickDuration   time.Duration
	)

	// Check if this routine needs to awake in this round and prepare essential
	// variables when yes.
	checkRound := func() (isNotary, isDisabled bool) {
		defer func() {
			currentRound = nextRound
			nextRound++
		}()
		// Wait until the configuartion for next round is ready.
		var config *agreementMgrConfig
		for {
			if config = mgr.getConfig(nextRound); config != nil {
				break
			} else {
				mgr.logger.Debug("round is not ready", "round", nextRound)
				time.Sleep(1 * time.Second)
			}
		}
		// Set next checkpoint.
		roundBeginTime = config.beginTime
		roundEndTime = config.beginTime.Add(config.roundInterval)
		// Check if this chain handled by this routine included in this round.
		if chainID >= config.numChains {
			isDisabled = true
			return
		}
		// Check if this node in notary set of this chain in this round.
		notarySet, err := mgr.cache.GetNotarySet(nextRound, chainID)
		if err != nil {
			panic(err)
		}
		setting.crs = config.crs
		setting.notarySet = notarySet
		_, isNotary = setting.notarySet[mgr.ID]
		if isNotary {
			mgr.logger.Info("selected as notary set",
				"ID", mgr.ID,
				"round", nextRound,
				"chainID", chainID)
		} else {
			mgr.logger.Info("not selected as notary set",
				"ID", mgr.ID,
				"round", nextRound,
				"chainID", chainID)
		}
		// Setup ticker
		if tickDuration != config.lambdaBA {
			if setting.ticker != nil {
				setting.ticker.Stop()
			}
			setting.ticker = newTicker(mgr.gov, nextRound, TickerBA)
			tickDuration = config.lambdaBA
		}
		return
	}
Loop:
	for {
		select {
		case <-mgr.ctx.Done():
			break Loop
		default:
		}
		now := time.Now().UTC()
		var isDisabled bool
		setting.recv.isNotary, isDisabled = checkRound()
		if isDisabled {
			select {
			case <-mgr.ctx.Done():
				break Loop
			case <-time.After(roundEndTime.Sub(now)):
				continue Loop
			}
		}
		// Sleep until round begin. Here a biased round begin time would be
		// used instead of the one in config. The reason it to disperse the load
		// of fullnodes to verify confirmed blocks from each chain.
		if now.Before(pickBiasedTime(roundBeginTime, 4*tickDuration)) {
			select {
			case <-mgr.ctx.Done():
				break Loop
			case <-time.After(roundBeginTime.Sub(now)):
			}
			// Clean the tick channel after awake: the tick would be queued in
			// channel, thus the first few ticks would not tick on expected
			// interval.
			<-setting.ticker.Tick()
			<-setting.ticker.Tick()
		}
		// Run BA for this round.
		recv.roundValue.Store(currentRound)
		recv.changeNotaryTime = roundEndTime
		recv.restartNotary <- types.Position{
			Round:   setting.recv.round(),
			ChainID: math.MaxUint32,
		}
		mgr.voteFilters[chainID] = utils.NewVoteFilter()
		if err := mgr.baRoutineForOneRound(&setting); err != nil {
			mgr.logger.Error("BA routine failed",
				"error", err,
				"nodeID", mgr.ID,
				"chain", chainID)
			break Loop
		}
	}
}

func (mgr *agreementMgr) baRoutineForOneRound(
	setting *baRoundSetting) (err error) {
	agr := setting.agr
	recv := setting.recv
	oldPos := agr.agreementID()
	restart := func(restartPos types.Position) (breakLoop bool, err error) {
		if !isStop(restartPos) {
			if restartPos.Round > oldPos.Round {
				for {
					select {
					case <-mgr.ctx.Done():
						break
					default:
					}
					tipRound := mgr.lattice.TipRound(setting.chainID)
					if tipRound > restartPos.Round {
						// It's a vary rare that this go routine sleeps for entire round.
						break
					} else if tipRound != restartPos.Round {
						mgr.logger.Debug("Waiting lattice to change round...",
							"pos", &restartPos)
					} else {
						break
					}
					time.Sleep(100 * time.Millisecond)
				}
				// This round is finished.
				breakLoop = true
				return
			}
			if restartPos.Older(&oldPos) {
				// The restartNotary event is triggered by 'BlockConfirmed'
				// of some older block.
				return
			}
		}
		var nextHeight uint64
		var nextTime time.Time
		for {
			nextHeight, nextTime, err =
				mgr.lattice.NextBlock(recv.round(), setting.chainID)
			if err != nil {
				mgr.logger.Debug("Error getting next height",
					"error", err,
					"round", recv.round(),
					"chainID", setting.chainID)
				err = nil
				nextHeight = restartPos.Height
			}
			if isStop(oldPos) && nextHeight == 0 {
				break
			}
			if isStop(restartPos) && nextHeight == 0 {
				break
			}
			if nextHeight > restartPos.Height {
				break
			}
			mgr.logger.Debug("Lattice not ready!!!",
				"old", &oldPos, "restart", &restartPos, "next", nextHeight)
			time.Sleep(100 * time.Millisecond)
		}
		nextPos := types.Position{
			Round:   recv.round(),
			ChainID: setting.chainID,
			Height:  nextHeight,
		}
		oldPos = nextPos
		var leader types.NodeID
		leader, err = mgr.cache.GetLeaderNode(nextPos)
		if err != nil {
			return
		}
		time.Sleep(nextTime.Sub(time.Now()))
		setting.ticker.Restart()
		agr.restart(setting.notarySet, nextPos, leader, setting.crs)
		return
	}
Loop:
	for {
		select {
		case <-mgr.ctx.Done():
			break Loop
		default:
		}
		if agr.confirmed() {
			// Block until receive restartPos
			select {
			case restartPos := <-recv.restartNotary:
				breakLoop, err := restart(restartPos)
				if err != nil {
					return err
				}
				if breakLoop {
					break Loop
				}
			case <-mgr.ctx.Done():
				break Loop
			}
		}
		select {
		case restartPos := <-recv.restartNotary:
			breakLoop, err := restart(restartPos)
			if err != nil {
				return err
			}
			if breakLoop {
				break Loop
			}
		default:
		}
		if agr.pullVotes() {
			pos := agr.agreementID()
			mgr.logger.Debug("Calling Network.PullVotes for syncing votes",
				"position", &pos)
			mgr.network.PullVotes(pos)
		}
		if err = agr.nextState(); err != nil {
			mgr.logger.Error("Failed to proceed to next state",
				"nodeID", mgr.ID.String(),
				"error", err)
			break Loop
		}
		for i := 0; i < agr.clocks(); i++ {
			// Priority select for agreement.done().
			select {
			case <-agr.done():
				continue Loop
			default:
			}
			select {
			case <-agr.done():
				continue Loop
			case <-setting.ticker.Tick():
			}
		}
	}
	return nil
}
