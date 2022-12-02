package endpoint

import (
	"aurora-relayer-go-common/endpoint"
	errs "aurora-relayer-go-common/types/errors"
	"context"
)

type EngineNet struct {
	*EngineEth
}

func NewEngineNet(eEth *EngineEth) *EngineNet {
	eNet := &EngineNet{eEth}
	return eNet
}

// Version returns the chain id of the current network. Therefore, directly calls the `chainId`` method under `engineEth` endpoint
func (e *EngineNet) Version(ctx context.Context) (*string, error) {
	return endpoint.Process(ctx, "net_version", e.Endpoint, func(ctx context.Context) (*string, error) {
		version, err := e.chainId(ctx)
		if err != nil {
			return nil, &errs.GenericError{Err: err}
		}
		base10str := version.Text(10)
		return &base10str, nil
	})
}
