package standaloneproxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/aurora-is-near/relayer2-base/cmdutils"
	"github.com/aurora-is-near/relayer2-base/endpoint"
	"github.com/aurora-is-near/relayer2-base/log"
	"github.com/aurora-is-near/relayer2-base/types/common"
	"github.com/aurora-is-near/relayer2-base/types/engine"
	"github.com/aurora-is-near/relayer2-base/types/response"
	"github.com/buger/jsonparser"
	"github.com/spf13/viper"
)

const configPath = "endpoint.standaloneproxy"

type RPCClient interface {
	TraceTransaction(hash common.H256) (*response.CallFrame, error)
	EstimateGas(tx engine.TransactionForCall, number *common.BN64) (*common.Uint256, error)
	Close() error
}

type Config struct {
	Network string        `mapstructure:"network"`
	Address string        `mapstructure:"address"`
	Timeout time.Duration `mapstructure:"timeout"`
}

func GetConfig() *Config {
	config := &Config{
		Network: "unix",
		Timeout: 5 * time.Second,
	}
	sub := viper.Sub(configPath)
	if sub != nil {
		cmdutils.BindSubViper(sub, configPath)
		if err := sub.Unmarshal(&config); err != nil {
			log.Log().Warn().Err(err).Msgf("failed to parse configuration [%s] from [%s], "+
				"falling back to defaults", configPath, viper.ConfigFileUsed())
		}
	}
	return config
}

type StandaloneProxy struct {
	Config *Config
	client RPCClient
}

func New() (*StandaloneProxy, error) {
	conf := GetConfig()
	client, err := newRPCClient(conf.Network, conf.Address, conf.Timeout)
	if err != nil {
		return nil, err
	}
	return &StandaloneProxy{conf, client}, err
}

func (l *StandaloneProxy) Close() error {
	return l.client.Close()
}

// Pre implements endpoint.Processor.
func (l *StandaloneProxy) Pre(ctx context.Context, name string, _ *endpoint.Endpoint, response *any, args ...any) (context.Context, bool, error) {
	switch name {
	case "debug_traceTransaction":
		if len(args) != 1 {
			return ctx, false, errors.New("invalid params")
		}
		hash, ok := args[0].(common.H256)
		if !ok {
			return ctx, false, errors.New("invalid params")
		}
		res, err := l.client.TraceTransaction(hash)
		if err != nil {
			return ctx, true, err
		}
		*response = res
		return ctx, true, nil

	case "eth_estimateGas":
		tx, ok := args[0].(engine.TransactionForCall)
		if !ok {
			return ctx, true, errors.New("invalid params")
		}
		blockNumberOrHash, ok := args[1].(*common.BlockNumberOrHash)
		if !ok {
			return ctx, true, errors.New("invalid params")
		}
		if blockNumberOrHash == nil {
			latest := common.LatestBlockNumber
			blockNumberOrHash = &common.BlockNumberOrHash{BlockNumber: &latest}
		}
		res, err := l.client.EstimateGas(tx, blockNumberOrHash.BlockNumber)
		if err != nil {
			return ctx, true, err
		}
		*response = res
		return ctx, true, nil

	default:
		return ctx, false, nil
	}
}

// Post implements endpoint.Processor.
func (*StandaloneProxy) Post(ctx context.Context, _ string, _ *any, _ *error) context.Context {
	return ctx
}

type rpcClient struct {
	conn    net.Conn
	lock    sync.Mutex
	network string
	address string
	timeout time.Duration
}

func newRPCClient(network string, address string, timeout time.Duration) (*rpcClient, error) {
	return &rpcClient{
		network: network,
		address: address,
		timeout: timeout,
	}, nil
}

func (rc *rpcClient) Close() error {
	if rc.conn == nil {
		return nil
	}
	return rc.conn.Close()
}

func (rc *rpcClient) TraceTransaction(hash common.H256) (*response.CallFrame, error) {
	req, err := buildRequest("debug_traceTransaction", hash)
	if err != nil {
		return nil, err
	}

	res, err := rc.request(req)
	if err != nil {
		return nil, err
	}

	result, resultType, _, err := jsonparser.Get(res, "result")
	if err != nil && !errors.Is(err, jsonparser.KeyPathNotFoundError) {
		return nil, err
	}

	switch resultType {
	case jsonparser.NotExist:
		rpcErr, rpcErrType, _, err := jsonparser.Get(res, "error", "message")
		if err != nil || rpcErrType != jsonparser.String {
			return nil, errors.New("internal rpc error")
		}
		return nil, fmt.Errorf("%s", rpcErr)

	case jsonparser.Object:
		trace := new(response.CallFrame)
		err := json.Unmarshal(result, trace)
		return trace, err

	case jsonparser.Array:
		traces := make([]*response.CallFrame, 0, 1)
		_, err := jsonparser.ArrayEach(result, func(value []byte, dataType jsonparser.ValueType, _ int, _ error) {
			trace := new(response.CallFrame)
			err = json.Unmarshal(value, trace)
			if err != nil {
				return
			}
			traces = append(traces, trace)
		})
		if len(traces) != 1 {
			return nil, errors.New("unexpected response")
		}
		return traces[0], err

	default:
		return nil, errors.New("failed to parse unexpected response")
	}
}

func (rc *rpcClient) EstimateGas(tx engine.TransactionForCall, number *common.BN64) (*common.Uint256, error) {
	var blockParam interface{} = number
	switch *number {
	case common.EarliestBlockNumber:
		blockParam = "earliest"
	case common.LatestBlockNumber:
		blockParam = "latest"
	case common.PendingBlockNumber:
		blockParam = "pending"
	}

	req, err := buildRequest("eth_estimateGas", tx, blockParam)
	if err != nil {
		return nil, fmt.Errorf("failed to build request: %w", err)
	}

	res, err := rc.request(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	return parseEstimateGasResponse(res)
}

func parseEstimateGasResponse(res []byte) (*common.Uint256, error) {
	result, resultType, _, err := jsonparser.Get(res, "result")

	if err == nil && resultType == jsonparser.Number {
		val, err := strconv.ParseInt(string(result), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to parse result as integer: %w", err)
		}
		hexStr := fmt.Sprintf("0x%x", val)

		resp := new(common.Uint256)
		if err := resp.UnmarshalJSON([]byte(hexStr)); err != nil {
			return nil, fmt.Errorf("failed to unmarshal result: %w", err)
		}
		return resp, nil
	}

	return handleEstimateGasError(res)
}

func handleEstimateGasError(res []byte) (*common.Uint256, error) {
	rpcErrData, _, _, rpcErrDataParseErr := jsonparser.Get(res, "error", "data")
	if rpcErrDataParseErr == nil && len(rpcErrData) > 0 {
		return nil, fmt.Errorf("engine error: %s", rpcErrData)
	}

	rpcErrMsg, _, _, rpcErrMsgParseErr := jsonparser.Get(res, "error", "message")
	if rpcErrMsgParseErr == nil && len(rpcErrMsg) > 0 {
		return nil, fmt.Errorf("engine error: %s", rpcErrMsg)
	}

	return nil, errors.New("engine error: unknown error occurred")
}

func (rc *rpcClient) reconnect() error {
	_, err := os.Stat(rc.address)
	if os.IsNotExist(err) {
		return errors.New("socket connection to refiner is not available. Trying to reconnect. Please try again")
	}
	c, err := net.Dial(rc.network, rc.address)
	if err != nil {
		return errors.New("socket connection to refiner is not available. Trying to reconnect. Please try again")
	} else {
		rc.conn = c
		return nil
	}
}

func (rc *rpcClient) request(req []byte) ([]byte, error) {
	if rc.conn == nil {
		if err := rc.reconnect(); err != nil {
			return nil, err
		}
	}
	rc.lock.Lock()
	defer rc.lock.Unlock()
	err := rc.conn.SetDeadline(time.Now().Add(rc.timeout))
	if err != nil {
		return nil, err
	}
	err = binary.Write(rc.conn, binary.LittleEndian, uint32(len(req)))
	if err != nil {
		if errors.Is(err, syscall.EPIPE) {
			if err := rc.reconnect(); err != nil {
				return nil, err
			}
		}
		return nil, err
	}

	_, err = rc.conn.Write(req)
	if err != nil {
		return nil, err
	}

	var l uint32
	err = binary.Read(rc.conn, binary.LittleEndian, &l)
	if err != nil {
		return nil, err
	}

	buf := make([]byte, l)
	_, err = io.ReadFull(rc.conn, buf)
	return buf, err
}

func buildRequest(method string, params ...any) ([]byte, error) {
	b := bytes.NewBufferString(`{"id":1,"jsonrpc":"2.0","method":"` + method + `","params":[`)
	first := true
	for _, param := range params {
		p, err := json.Marshal(param)
		if err != nil {
			return nil, err
		}

		if !first {
			b.WriteByte(',')
		}
		first = false

		b.Write(p)
	}
	b.WriteString("]}")
	return b.Bytes(), nil
}
