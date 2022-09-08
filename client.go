// package client is responsible for easily interacting with the protobuf API in Go.
// It contains wrappers for all the rpc methods that accept standard go-ethereum
// objects.
package client

import (
	"context"
	"fmt"
	"math/big"

	"github.com/chainbound/fiber-go/protobuf/api"
	"github.com/chainbound/fiber-go/protobuf/eth"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type Client struct {
	target string
	conn   *grpc.ClientConn
	client api.APIClient
}

func NewClient(target string) *Client {
	return &Client{
		target: target,
	}
}

// Connects sets up the gRPC channel and creates the stub. It blocks until connected or the given context expires.
// Always use a context with timeout.
func (c *Client) Connect(ctx context.Context) error {
	conn, err := grpc.DialContext(ctx, c.target, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		return err
	}

	c.conn = conn

	// Create the stub (client) with the channel
	c.client = api.NewAPIClient(conn)
	return nil
}

func (c *Client) Close() error {
	return c.conn.Close()
}

// SendTransaction sends the (signed) transaction to Fibernet and returns the hash and a timestamp (us).
// It blocks until the transaction was sent.
func (c *Client) SendTransaction(ctx context.Context, tx *types.Transaction) (string, int64, error) {
	proto, err := TxToProto(tx)
	if err != nil {
		return "", 0, fmt.Errorf("converting to protobuf: %w", err)
	}

	res, err := c.client.SendTransaction(ctx, proto)
	if err != nil {
		return "", 0, fmt.Errorf("sending tx to api: %w", err)
	}

	return res.Hash, res.Timestamp, nil
}

func (c *Client) BackrunTransaction(ctx context.Context, hash common.Hash, tx *types.Transaction) (string, int64, error) {
	proto, err := TxToProto(tx)
	if err != nil {
		return "", 0, fmt.Errorf("converting to protobuf: %w", err)
	}

	res, err := c.client.Backrun(ctx, &api.BackrunMsg{
		Hash: hash.String(),
		Tx:   proto,
	})

	if err != nil {
		return "", 0, fmt.Errorf("sending backrun to api: %w", err)
	}

	return res.Hash, res.Timestamp, nil
}

// SubscribeNewTxs subscribes to new transactions, and sends transactions on the given
// channel according to the filter. This function blocks and should be called in a goroutine.
// If there's an error receiving the new message it will close the channel and return the error.
func (c *Client) SubscribeNewTxs(filter *api.TxFilter, ch chan<- *types.Transaction) error {
	res, err := c.client.SubscribeNewTxs(context.Background(), filter)
	if err != nil {
		return fmt.Errorf("subscribing to api: %w", err)
	}

	for {
		proto, err := res.Recv()
		if err != nil {
			close(ch)
			return err
		}

		ch <- ProtoToTx(proto)
	}
}

func (c *Client) SubscribeNewBlocks(filter *api.BlockFilter, ch chan<- *eth.Block) error {
	res, err := c.client.SubscribeNewBlocks(context.Background(), filter)
	if err != nil {
		return fmt.Errorf("subscribing to api: %w", err)
	}

	for {
		proto, err := res.Recv()
		if err != nil {
			close(ch)
			return err
		}

		ch <- proto
	}
}

func TxToProto(tx *types.Transaction) (*eth.Transaction, error) {
	signer := types.NewLondonSigner(common.Big1)
	sender, err := types.Sender(signer, tx)
	if err != nil {
		return nil, err
	}

	var to []byte
	if tx.To() != nil {
		to = tx.To().Bytes()
	}

	v, r, s := tx.RawSignatureValues()
	return &eth.Transaction{
		ChainId:     uint32(tx.ChainId().Uint64()),
		To:          to,
		Gas:         tx.Gas(),
		GasPrice:    tx.GasPrice().Uint64(),
		MaxFee:      tx.GasFeeCap().Uint64(),
		PriorityFee: tx.GasTipCap().Uint64(),
		Hash:        tx.Hash().Bytes(),
		Input:       tx.Data(),
		Nonce:       tx.Nonce(),
		Value:       tx.Value().Bytes(),
		From:        sender.Bytes(),
		Type:        uint32(tx.Type()),
		V:           v.Uint64(),
		R:           r.Bytes(),
		S:           s.Bytes(),
	}, nil
}

// ProtoToTx converts a protobuf transaction to a go-ethereum transaction.
// It does not include the AccessList.
func ProtoToTx(proto *eth.Transaction) *types.Transaction {
	to := new(common.Address)

	if len(proto.To) > 0 {
		to = (*common.Address)(proto.To)
	}
	switch proto.Type {
	case 0:
		return types.NewTx(&types.LegacyTx{
			Nonce:    proto.Nonce,
			GasPrice: big.NewInt(int64(proto.GasPrice)),
			Gas:      proto.Gas,
			To:       to,
			Value:    new(big.Int).SetBytes(proto.Value),
			Data:     proto.Input,
			V:        big.NewInt(int64(proto.V)),
			R:        new(big.Int).SetBytes(proto.R),
			S:        new(big.Int).SetBytes(proto.S),
		})
	case 1:
		return types.NewTx(&types.AccessListTx{
			ChainID:  big.NewInt(int64(proto.ChainId)),
			Nonce:    proto.Nonce,
			GasPrice: big.NewInt(int64(proto.GasPrice)),
			Gas:      proto.Gas,
			To:       to,
			Value:    new(big.Int).SetBytes(proto.Value),
			Data:     proto.Input,
			V:        big.NewInt(int64(proto.V)),
			R:        new(big.Int).SetBytes(proto.R),
			S:        new(big.Int).SetBytes(proto.S),
		})
	case 2:
		return types.NewTx(&types.DynamicFeeTx{
			ChainID:   big.NewInt(int64(proto.ChainId)),
			Nonce:     proto.Nonce,
			GasFeeCap: big.NewInt(int64(proto.MaxFee)),
			GasTipCap: big.NewInt(int64(proto.PriorityFee)),
			Gas:       proto.Gas,
			To:        to,
			Value:     new(big.Int).SetBytes(proto.Value),
			Data:      proto.Input,
			V:         big.NewInt(int64(proto.V)),
			R:         new(big.Int).SetBytes(proto.R),
			S:         new(big.Int).SetBytes(proto.S),
		})
	}

	return nil
}