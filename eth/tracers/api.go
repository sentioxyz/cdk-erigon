package tracers

import (
	"encoding/json"

	libcommon "github.com/erigontech/erigon-lib/common"
	"github.com/erigontech/erigon-lib/common/hexutil"
	"github.com/erigontech/erigon-lib/common/hexutility"
	"github.com/erigontech/erigon/eth/tracers/logger"
	"github.com/erigontech/erigon/turbo/adapter/ethapi"
)

// TraceConfig holds extra parameters to trace functions.
type TraceConfig struct {
	*logger.LogConfig
	Tracer         *string
	TracerConfig   *json.RawMessage
	Timeout        *string
	Reexec         *uint64
	NoRefunds      *bool // Turns off gas refunds when tracing
	StateOverrides *ethapi.StateOverrides

	IgnoreGas             *bool
	IgnoreCodeSizeLimit   *bool
	CreationCodeOverrides map[libcommon.Address]hexutility.Bytes
	CreateAddressOverride *libcommon.Address

	BorTraceEnabled *bool
	TxIndex         *hexutil.Uint
}
