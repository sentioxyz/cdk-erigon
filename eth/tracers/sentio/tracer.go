package sentio

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sync/atomic"

	"github.com/holiman/uint256"
	libcommon "github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/common/hexutil"
	"github.com/ledgerwatch/erigon-lib/common/hexutility"
	"github.com/ledgerwatch/erigon/accounts/abi"
	"github.com/ledgerwatch/erigon/common"
	"github.com/ledgerwatch/erigon/common/math"
	corestate "github.com/ledgerwatch/erigon/core/state"
	"github.com/ledgerwatch/erigon/core/vm"
	"github.com/ledgerwatch/erigon/core/vm/stack"
	"github.com/ledgerwatch/erigon/eth/tracers"
	"github.com/ledgerwatch/log/v3"
)

type functionInfo struct {
	address       string
	Name          string `json:"name"`
	SignatureHash string `json:"signatureHash"`

	Pc           uint64 `json:"pc"`
	InputSize    int    `json:"inputSize"`
	InputMemory  bool   `json:"inputMemory"`
	OutputSize   int    `json:"outputSize"`
	OutputMemory bool   `json:"outputMemory"`
}

type sentioTracerConfig struct {
	Functions         map[string][]functionInfo `json:"functions"`
	Calls             map[string][]uint64       `json:"calls"`
	Debug             bool                      `json:"debug"`
	WithInternalCalls bool                      `json:"withInternalCalls"`
}

func init() {
	tracers.RegisterLookup(false, newSentioTracer)
}

type Trace struct {
	//	only in debug mode
	Name string `json:"name,omitempty"`

	Type string `json:"type"`
	Pc   uint64 `json:"pc"`
	// Global index of the trace
	StartIndex int `json:"startIndex"`
	EndIndex   int `json:"endIndex"`

	// Gas remaining before the OP
	Gas math.HexOrDecimal64 `json:"gas"`
	// Gas for the entire call
	GasUsed math.HexOrDecimal64 `json:"gasUsed"`

	From *libcommon.Address `json:"from,omitempty"`
	// Used by call
	To *libcommon.Address `json:"to,omitempty"`
	// Input
	Input string `json:"input,omitempty"` // TODO better struct it and make it bytes
	// Ether transfered
	Value *hexutil.Big `json:"value,omitempty"`
	// Return for calls
	Output   hexutility.Bytes `json:"output,omitempty"`
	Error    string           `json:"error,omitempty"`
	Revertal string           `json:"revertReason,omitempty"`

	// Used by jump
	InputStack   []string  `json:"inputStack,omitempty"`
	InputMemory  *[]string `json:"inputMemory,omitempty"`
	OutputStack  []string  `json:"outputStack,omitempty"`
	OutputMemory *[]string `json:"outputMemory,omitempty"`
	FunctionPc   uint64    `json:"functionPc,omitempty"`

	// Used by log
	Address     *libcommon.Address `json:"address,omitempty"`
	CodeAddress *libcommon.Address `json:"codeAddress,omitempty"`
	Data        hexutility.Bytes   `json:"data,omitempty"`

	Topics []libcommon.Hash `json:"topics,omitempty"`

	// Only used by root
	Traces []Trace `json:"traces,omitempty"`

	// Use for internal call stack organization
	// The jump to go into the function
	//enterPc uint64
	exitPc uint64

	// the function get called
	function *functionInfo
}

type Receipt struct {
	Nonce            uint64          `json:"nonce"`
	TxHash           *libcommon.Hash `json:"transactionHash,omitempty"`
	BlockNumber      *hexutil.Big    `json:"blockNumber,omitempty"`
	BlockHash        *libcommon.Hash `json:"blockHash,omitempty"`
	TransactionIndex uint            `json:"transactionIndex"`
	GasPrice         *hexutil.Big    `json:"gasPrice,omitempty"`
}

type sentioTracer struct {
	config      sentioTracerConfig
	env         *vm.EVM
	functionMap map[string]map[uint64]functionInfo
	callMap     map[string]map[uint64]bool
	receipt     Receipt

	previousJump *Trace
	index        int
	entryPc      map[uint64]bool

	callstack []Trace
	gasLimit  uint64

	interrupt uint32 // Atomic flag to signal execution interruption
	reason    error  // Textual reason for the interruption
}

func (t *sentioTracer) CaptureTxStart(gasLimit uint64) {
	t.gasLimit = gasLimit
}

func (t *sentioTracer) CaptureTxEnd(restGas uint64) {
	if len(t.callstack) == 0 {
		return
	}
	t.callstack[0].EndIndex = t.index
	t.callstack[0].GasUsed = math.HexOrDecimal64(t.gasLimit - restGas)
	if t.callstack[0].StartIndex == -1 {
		// It's possible that we can't correctly locate the PC that match the entry function (check why), in this case we need to 0 for the user
		t.callstack[0].StartIndex = 0
	}
}

func (t *sentioTracer) CaptureStart(env *vm.EVM, from libcommon.Address, to libcommon.Address, precompile bool, create bool, input []byte, gas uint64, value *uint256.Int, code []byte) {
	t.env = env
	t.receipt.BlockNumber = (*hexutil.Big)(big.NewInt(int64(env.Context.BlockNumber)))
	//if env.Context().GetHash != nil {
	// TODO this current will block the tracer
	//	h := env.Context().GetHash(env.Context().BlockNumber)
	//	t.receipt.BlockHash = &h
	//}
	if (env.TxContext.TxHash != libcommon.Hash{}) {
		h := t.env.TxContext.TxHash
		t.receipt.TxHash = &h
	}
	t.receipt.GasPrice = (*hexutil.Big)(env.GasPrice.ToBig())
	t.receipt.Nonce = env.IntraBlockState().GetNonce(from) - 1
	if ibs, ok := env.IntraBlockState().(*corestate.IntraBlockState); ok {
		t.receipt.TransactionIndex = uint(ibs.TxIndex())
		bHash := ibs.BlockHash()
		t.receipt.BlockHash = &bHash
	}
	root := Trace{
		StartIndex: -1,
		Type:       vm.CALL.String(),
		From:       &from,
		To:         &to,
		Gas:        math.HexOrDecimal64(gas),
		Input:      hexutility.Bytes(input).String(),
	}
	if value != nil {
		root.Value = (*hexutil.Big)(value.ToBig())
	}
	if create {
		root.Type = vm.CREATE.String()
	}

	if !create && !precompile && len(input) >= 4 {
		m, ok := t.functionMap[to.String()]
		if ok {
			sigHash := "0x" + common.Bytes2Hex(input[0:4])
			for pc, fn := range m {
				if fn.SignatureHash == sigHash {
					t.entryPc[pc] = true
				}
			}
			log.Info(fmt.Sprintf("entry pc match %s (%d times) ", sigHash, len(t.entryPc)))
		}
	}
	t.callstack = append(t.callstack, root)
}

func (t *sentioTracer) CaptureEnd(output []byte, usedGas uint64, err error) {
	t.callstack[0].EndIndex = t.index
	t.callstack[0].GasUsed = math.HexOrDecimal64(usedGas)
	t.callstack[0].Output = libcommon.CopyBytes(output)

	stackSize := len(t.callstack)
	t.popStack(1, output, uint64(t.callstack[stackSize-1].Gas)-usedGas, err)

	t.callstack[0].processError(output, err)
}

func (t *sentioTracer) CaptureEnter(typ vm.OpCode, from libcommon.Address, to libcommon.Address, precompile bool, create bool, input []byte, gas uint64, value *uint256.Int, code []byte) {
	// Skip if tracing was interrupted
	if atomic.LoadUint32(&t.interrupt) > 0 {
		return
	}

	if typ == vm.CALL || typ == vm.CALLCODE {
		// After enter, make the assumped transfer as function call
		topElementTraces := t.callstack[len(t.callstack)-1].Traces
		call := topElementTraces[len(topElementTraces)-1]
		topElementTraces = topElementTraces[:len(topElementTraces)-1]
		t.callstack[len(t.callstack)-1].Traces = topElementTraces
		t.callstack = append(t.callstack, call)
	}

	size := len(t.callstack)

	t.callstack[size-1].From = &from
	t.callstack[size-1].To = &to
	t.callstack[size-1].Input = hexutility.Bytes(input).String()
	t.callstack[size-1].Gas = math.HexOrDecimal64(gas)

	if value != nil {
		t.callstack[size-1].Value = (*hexutil.Big)(value.ToBig())
	}
}

func (t *sentioTracer) CaptureExit(output []byte, usedGas uint64, err error) {
	size := len(t.callstack)
	if size <= 1 {
		return
	}

	//log.Info(fmt.Sprintf("CaptureExit pop frame %s", t.callstack[size-1].Type))

	stackSize := len(t.callstack)
	for i := stackSize - 1; i >= 0; i-- {
		if t.callstack[i].function != nil {
			continue
		}

		if stackSize-i > 1 {
			log.Info(fmt.Sprintf("tail call optimization [external] size %d", stackSize-i))
		}

		call := &t.callstack[i]
		//call.EndIndex = t.index
		//call.GasUsed = math.HexOrDecimal64(usedGas)
		call.processError(output, err)

		t.popStack(i, output, uint64(call.Gas)-usedGas, err)
		return
	}

	log.Error(fmt.Sprintf("failed to pop stack"))
}

func (t *sentioTracer) popStack(to int, output []byte, currentGas uint64, err error) { // , scope *vm.ScopeContext
	stackSize := len(t.callstack)
	for j := stackSize - 1; j >= to; j-- {
		t.callstack[j].Output = libcommon.CopyBytes(output)
		t.callstack[j].EndIndex = t.index
		t.callstack[j].GasUsed = math.HexOrDecimal64(uint64(t.callstack[j].Gas) - currentGas)

		// TODO consider pass scopeContext so that popStack also record this
		//if t.callstack[j].function != nil {
		//	t.callstack[j].OutputStack = copyStack(scope.Stack, t.callstack[j].function.OutputSize)
		//	if t.callstack[j].function.OutputMemory {
		//		t.callstack[j].OutputMemory = formatMemory(scope.Memory)
		//	}
		//}
		//if err != nil {
		//	t.callstack[j].Error = err.Error()
		//}
		t.callstack[j-1].Traces = append(t.callstack[j-1].Traces, t.callstack[j])
	}

	t.callstack = t.callstack[:to]
}

func (t *sentioTracer) CaptureState(pc uint64, op vm.OpCode, gas, cost uint64, scope *vm.ScopeContext, rData []byte, depth int, err error) {
	// Skip if tracing was interrupted
	if atomic.LoadUint32(&t.interrupt) > 0 {
		return
	}
	t.index++

	if t.callstack[0].StartIndex == -1 && t.entryPc[pc] {
		//fillback the index and PC for root
		t.callstack[0].Pc = pc
		t.callstack[0].StartIndex = t.index - 1
		t.previousJump = nil
		return
	}

	var mergeBase = func(trace Trace) Trace {
		trace.Pc = pc
		trace.Type = op.String()
		trace.Gas = math.HexOrDecimal64(gas)
		trace.StartIndex = t.index - 1
		trace.EndIndex = t.index

		// Assume it's single instruction, adjust it for jump and call
		trace.GasUsed = math.HexOrDecimal64(cost)
		if err != nil {
			// set error for instruction
			trace.Error = err.Error()
		}
		return trace
	}

	switch op {
	case vm.CALL, vm.CALLCODE:
		call := mergeBase(Trace{})

		call.Gas = math.HexOrDecimal64(scope.Stack.Back(0).Uint64())
		from := scope.Contract.Address()
		call.From = &from
		call.CodeAddress = scope.Contract.CodeAddr
		to := libcommon.BigToAddress(scope.Stack.Back(1).ToBig())
		call.To = &to
		call.Value = (*hexutil.Big)(scope.Stack.Back(2).ToBig())

		v, _ := uint256.FromBig(call.Value.ToInt())
		if !v.IsZero() && !t.env.Context.CanTransfer(t.env.IntraBlockState(), from, v) {
			if call.Error == "" {
				call.Error = "insufficient funds for transfer"
			}
		}

		// Treat this call as pure transfer until it enters the CaptureEnter
		t.callstack[len(t.callstack)-1].Traces = append(t.callstack[len(t.callstack)-1].Traces, call)
	case vm.CREATE, vm.CREATE2, vm.DELEGATECALL, vm.STATICCALL, vm.SELFDESTRUCT:
		// more info to be add at CaptureEnter
		call := mergeBase(Trace{})
		t.callstack = append(t.callstack, call)
	case vm.LOG0, vm.LOG1, vm.LOG2, vm.LOG3, vm.LOG4:
		topicCount := int(op - vm.LOG0)
		logOffset := scope.Stack.Peek()
		logSize := scope.Stack.Back(1)
		data := copyMemory(scope.Memory, logOffset, logSize)
		var topics []libcommon.Hash
		//stackLen := scope.Stack.Len()
		for i := 0; i < topicCount; i++ {
			topics = append(topics, scope.Stack.Back(2+i).Bytes32())
		}
		addr := scope.Contract.Address()
		l := mergeBase(Trace{
			Address:     &addr,
			CodeAddress: scope.Contract.CodeAddr,
			Data:        data,
			Topics:      topics,
		})
		t.callstack[len(t.callstack)-1].Traces = append(t.callstack[len(t.callstack)-1].Traces, l)
	case vm.JUMP:
		if !t.config.WithInternalCalls {
			break
		}
		from := scope.Contract.CodeAddr
		codeAddress := scope.Contract.CodeAddr

		jump := mergeBase(Trace{
			From:        from,
			CodeAddress: codeAddress,
			//InputStack: append([]uint256.Int(nil), scope.Stack.Data...), // TODO only need partial
		})
		if t.previousJump != nil {
			log.Error("Unexpected previous jump", t.previousJump)
		}
		if err == nil {
			t.previousJump = &jump
		} else {
			log.Error("error in jump", "err", err)
			// error happend, attach to current frame
			t.callstack[len(t.callstack)-1].Traces = append(t.callstack[len(t.callstack)-1].Traces, jump)
		}
	case vm.JUMPDEST:
		if !t.config.WithInternalCalls {
			break
		}
		from := scope.Contract.CodeAddr
		fromStr := from.String()

		if t.previousJump != nil { // vm.JumpDest and match with a previous jump (otherwise it's a jumpi)
			defer func() {
				t.previousJump = nil
			}()
			// Check if this is return
			// TODO pontentially maintain a map for fast filtering
			//log.Info("fromStr" + fromStr + ", callstack size" + fmt.Sprint(len(t.callStack)))
			stackSize := len(t.callstack)

			// Part 1: try process the trace as function call exit
			for i := stackSize - 1; i >= 0; i-- {
				// process internal call within the same contract
				// no function info means another external call
				functionInfo := t.callstack[i].function
				if functionInfo == nil {
					break
				}

				if functionInfo.address != fromStr {
					break
				}

				// find a match
				if t.callstack[i].exitPc == pc {
					// find a match, pop the stack, copy memory if needed

					if stackSize-i > 1 {
						log.Info(fmt.Sprintf("tail call optimization size %d", stackSize-i))
					}

					// TODO maybe don't need return all
					for j := stackSize - 1; j >= i; j-- {
						call := &t.callstack[j]
						functionJ := call.function
						call.EndIndex = t.index - 1 // EndIndex should before the jumpdest
						call.GasUsed = math.HexOrDecimal64(uint64(t.callstack[j].Gas) - gas)
						if functionJ.OutputSize > scope.Stack.Len() {
							log.Error(fmt.Sprintf("stack size not enough (%d vs %d) for function %s %s. pc: %d",
								scope.Stack.Len(), functionJ.OutputSize, functionJ.address, functionJ.Name, pc))
							if err == nil {
								log.Error("stack size not enough has error", "err", err)
							}
						} else {
							call.OutputStack = copyStack(scope.Stack, t.callstack[j].function.OutputSize)
						}
						if call.function.OutputMemory {
							call.OutputMemory = formatMemory(scope.Memory)
						}
						//if err != nil {
						//	call.Error = err.Error()
						//}
						t.callstack[j-1].Traces = append(t.callstack[j-1].Traces, *call)
					}
					t.callstack = t.callstack[:i]
					return
				}
			}

			// Part 2: try process the trace as function call entry
			funcInfo := t.getFunctionInfo(fromStr, pc)
			//log.Info("function info" + fmt.Sprint(funcInfo))

			if funcInfo != nil {
				// filter those jump are not call site
				if !t.isCall(t.previousJump.From.String(), t.previousJump.Pc) {
					return
				}

				if funcInfo.InputSize >= scope.Stack.Len() {
					// TODO this check should not needed after frist check
					log.Error("Unexpected stack size for function:" + fmt.Sprint(funcInfo) + ", stack" + fmt.Sprint(scope.Stack.Data))
					log.Error("previous jump" + fmt.Sprint(*t.previousJump))
					return
				}

				// confirmed that we are in an internal call
				//t.internalCallStack = append(t.internalCallStack, internalCallStack{
				//	enterPc:  t.previousJump.Pc,
				//	exitPc:   scope.Stack.Back(funcInfo.InputSize).Uint64(),
				//	function: funcInfo,
				//})
				//jump.enterPc = t.previousJump.Pc

				t.previousJump.exitPc = scope.Stack.Back(funcInfo.InputSize).Uint64()
				t.previousJump.function = funcInfo
				t.previousJump.FunctionPc = pc
				t.previousJump.InputStack = copyStack(scope.Stack, funcInfo.InputSize)
				if t.config.Debug {
					t.previousJump.Name = funcInfo.Name
				}
				if funcInfo.InputMemory {
					t.previousJump.InputMemory = formatMemory(scope.Memory)
				}
				t.callstack = append(t.callstack, *t.previousJump)
				//t.callstack = append(t.callstack, callStack{
			}
		}
	case vm.REVERT:
		if !t.config.WithInternalCalls {
			break
		}
		logOffset := scope.Stack.Peek()
		logSize := scope.Stack.Back(1)
		output := scope.Memory.GetPtr(int64(logOffset.Uint64()), int64(logSize.Uint64()))
		//data := copyMemory(logOffset, logSize)

		trace := mergeBase(Trace{
			Output: output,
			Error:  "execution reverted",
		})
		if unpacked, err := abi.UnpackRevert(output); err == nil {
			trace.Revertal = unpacked
		}
		t.callstack[len(t.callstack)-1].Traces = append(t.callstack[len(t.callstack)-1].Traces, trace)
	default:
		if !t.config.WithInternalCalls {
			break
		}
		if err != nil {
			// Error happen, attach the error OP if not already processed
			t.callstack[len(t.callstack)-1].Traces = append(t.callstack[len(t.callstack)-1].Traces, mergeBase(Trace{}))
		}
	}
}

func (t *sentioTracer) CaptureFault(pc uint64, op vm.OpCode, gas, cost uint64, scope *vm.ScopeContext, depth int, err error) {
}

// CapturePreimage records a SHA3 preimage discovered during execution.
func (t *sentioTracer) CapturePreimage(pc uint64, hash libcommon.Hash, preimage []byte) {}

func (t *sentioTracer) GetResult() (json.RawMessage, error) {
	type RootTrace struct {
		Trace
		TracerConfig *sentioTracerConfig `json:"tracerConfig,omitempty"`
		Receipt      Receipt             `json:"receipt"`
	}
	root := RootTrace{
		Trace:   t.callstack[0],
		Receipt: t.receipt,
	}

	if t.config.Debug {
		root.TracerConfig = &t.config
	}

	if len(t.callstack) != 1 {
		log.Error("callstack length is not 1, is " + fmt.Sprint(len(t.callstack)))
	}

	res, err := json.Marshal(root)
	if err != nil {
		return nil, err
	}
	return res, t.reason
}

func (t *sentioTracer) Stop(err error) {
	t.reason = err
	atomic.StoreUint32(&t.interrupt, 1)
}

func newSentioTracer(name string, ctx *tracers.Context, cfg json.RawMessage) (tracers.Tracer, error) {
	if name != "sentioTracer" {
		return nil, errors.New("no tracer found")
	}

	var config sentioTracerConfig
	functionMap := map[string]map[uint64]functionInfo{}
	callMap := map[string]map[uint64]bool{}

	if cfg != nil {
		if err := json.Unmarshal(cfg, &config); err != nil {
			return nil, err
		}

		for address, functions := range config.Functions {
			checkSumAddress := libcommon.HexToAddress(address).String()
			functionMap[checkSumAddress] = make(map[uint64]functionInfo)

			for _, function := range functions {
				function.address = checkSumAddress
				functionMap[checkSumAddress][function.Pc] = function
			}
		}

		for address, calls := range config.Calls {
			checkSumAddress := libcommon.HexToAddress(address).String()
			callMap[checkSumAddress] = make(map[uint64]bool)

			for _, call := range calls {
				callMap[checkSumAddress][call] = true
			}
		}

		log.Info(fmt.Sprintf("create sentioTracer config with %d functions, %d calls", len(functionMap), len(callMap)))
	}

	return &sentioTracer{
		config:      config,
		functionMap: functionMap,
		callMap:     callMap,
		entryPc:     map[uint64]bool{},
	}, nil
}

//func (t *sentioTracer) isPrecompiled(addr libcommon.Address) bool {
//	for _, p := range t.activePrecompiles {
//		if p == addr {
//			return true
//		}
//	}
//	return false
//}

func (t *sentioTracer) getFunctionInfo(address string, pc uint64) *functionInfo {
	m, ok := t.functionMap[address]
	if !ok || m == nil {
		return nil
	}
	info, ok := m[pc]
	if ok {
		return &info
	}

	return nil
}

func (t *sentioTracer) isCall(address string, pc uint64) bool {
	m, ok := t.callMap[address]
	if !ok || m == nil {
		return false
	}
	info, ok := m[pc]
	if ok {
		return info
	}
	return false
}

// Only used in non detail mode
func (f *Trace) processError(output []byte, err error) {
	//output = common.CopyBytes(output)
	if err == nil {
		//f.Output = output
		return
	}
	f.Error = err.Error()
	if f.Type == vm.CREATE.String() || f.Type == vm.CREATE2.String() {
		f.To = &libcommon.Address{}
	}
	if !errors.Is(err, vm.ErrExecutionReverted) || len(output) == 0 {
		return
	}
	//f.Output = output
	if len(output) < 4 {
		return
	}
	if unpacked, err := abi.UnpackRevert(output); err == nil {
		f.Revertal = unpacked
	}
}

func copyMemory(m *vm.Memory, offset *uint256.Int, size *uint256.Int) hexutility.Bytes {
	// it's important to get copy
	return m.GetCopy(int64(offset.Uint64()), int64(size.Uint64()))
}

func formatMemory(m *vm.Memory) *[]string {
	res := make([]string, 0, (m.Len()+31)/32)
	for i := 0; i+32 <= m.Len(); i += 32 {
		res = append(res, fmt.Sprintf("%x", m.GetPtr(int64(i), 32)))
	}
	return &res
}

func copyStack(s *stack.Stack, copySize int) []string {
	if copySize == 0 {
		return nil
	}
	stackSize := s.Len()
	res := make([]string, stackSize)
	for i := stackSize - copySize; i < stackSize; i++ {
		res[i] = s.Data[i].Hex()
	}
	return res
}
