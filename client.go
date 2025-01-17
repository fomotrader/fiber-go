// package client is responsible for easily interacting with the protobuf API in Go.
// It contains wrappers for all the rpc methods that accept standard go-ethereum
// objects.
package client

import (
	"context"
	"fmt"

	"github.com/chainbound/fiber-go/filter"
	"github.com/chainbound/fiber-go/protobuf/api"
	"github.com/chainbound/fiber-go/protobuf/eth"

	"github.com/ethereum/go-ethereum/core/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/emptypb"
)

type Client struct {
	target string
	conn   *grpc.ClientConn
	client api.APIClient
	key    string

	// streams
	txStream       api.API_SendTransactionClient
	rawTxStream    api.API_SendRawTransactionClient
	txSeqStream    api.API_SendTransactionSequenceClient
	rawTxSeqStream api.API_SendRawTransactionSequenceClient
}

func NewClient(target, apiKey string) *Client {
	return &Client{
		target: target,
		key:    apiKey,
	}
}

// Connects sets up the gRPC channel and creates the stub. It blocks until connected or the given context expires.
// Always use a context with timeout.
func (c *Client) Connect(ctx context.Context) error {
	conn, err := grpc.DialContext(ctx, c.target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithReadBufferSize(0),
		grpc.WithWriteBufferSize(0),
	)
	if err != nil {
		return err
	}

	c.conn = conn

	// Create the stub (client) with the channel
	c.client = api.NewAPIClient(conn)

	ctx = metadata.AppendToOutgoingContext(context.Background(), "x-api-key", c.key)
	c.txStream, err = c.client.SendTransaction(ctx)
	if err != nil {
		return err
	}

	c.rawTxStream, err = c.client.SendRawTransaction(ctx)
	if err != nil {
		return err
	}

	c.txSeqStream, err = c.client.SendTransactionSequence(ctx)
	if err != nil {
		return err
	}

	c.rawTxSeqStream, err = c.client.SendRawTransactionSequence(ctx)
	if err != nil {
		return err
	}

	return nil
}

// Close closes all the streams and then the underlying connection. IMPORTANT: you should call this
// to ensure correct API accounting.
func (c *Client) Close() error {
	c.txStream.CloseSend()
	c.rawTxStream.CloseSend()
	c.txSeqStream.CloseSend()
	c.rawTxSeqStream.CloseSend()

	return c.conn.Close()
}

// SendTransaction sends the (signed) transaction to Fibernet and returns the hash and a timestamp (us).
// It blocks until the transaction was sent.
func (c *Client) SendTransaction(ctx context.Context, tx *types.Transaction) (string, int64, error) {
	proto, err := TxToProto(tx)
	if err != nil {
		return "", 0, fmt.Errorf("converting to protobuf: %w", err)
	}

	errc := make(chan error)
	go func() {
		if err := c.txStream.Send(proto); err != nil {
			errc <- err
		}
	}()

	for {
		select {
		case err := <-errc:
			return "", 0, err
		default:
		}

		res, err := c.txStream.Recv()
		if err != nil {
			return "", 0, err
		} else {
			return res.Hash, res.Timestamp, nil
		}
	}
}

func (c *Client) SendRawTransaction(ctx context.Context, rawTx []byte) (string, int64, error) {
	errc := make(chan error)
	go func() {
		if err := c.rawTxStream.Send(&api.RawTxMsg{RawTx: rawTx}); err != nil {
			errc <- err
		}
	}()

	for {
		select {
		case err := <-errc:
			return "", 0, err
		default:
		}

		res, err := c.rawTxStream.Recv()
		if err != nil {
			return "", 0, err
		} else {
			return res.Hash, res.Timestamp, nil
		}
	}
}

func (c *Client) SendTransactionSequence(ctx context.Context, transactions ...*types.Transaction) ([]string, int64, error) {
	errc := make(chan error)

	protoSeq := make([]*eth.Transaction, len(transactions))

	for i, tx := range transactions {
		proto, err := TxToProto(tx)
		if err != nil {
			return nil, 0, err
		}

		protoSeq[i] = proto
	}

	go func() {
		if err := c.txSeqStream.Send(&api.TxSequenceMsg{Sequence: protoSeq}); err != nil {
			errc <- err
		}
	}()

	for {
		select {
		case err := <-errc:
			return nil, 0, err
		default:
		}

		res, err := c.txSeqStream.Recv()
		if err != nil {
			return nil, 0, err
		}

		hashes := make([]string, len(res.SequenceResponse))
		ts := res.SequenceResponse[0].Timestamp

		for i, response := range res.SequenceResponse {
			hashes[i] = response.Hash
		}

		return hashes, ts, nil
	}
}

func (c *Client) SendRawTransactionSequence(ctx context.Context, rawTransactions ...[]byte) ([]string, int64, error) {
	errc := make(chan error)

	go func() {
		if err := c.rawTxSeqStream.Send(&api.RawTxSequenceMsg{RawTxs: rawTransactions}); err != nil {
			errc <- err
		}
	}()

	for {
		select {
		case err := <-errc:
			return nil, 0, err
		default:
		}

		res, err := c.rawTxSeqStream.Recv()
		if err != nil {
			return nil, 0, err
		}

		hashes := make([]string, len(res.SequenceResponse))

		ts := res.SequenceResponse[0].Timestamp

		for i, response := range res.SequenceResponse {
			hashes[i] = response.Hash
		}

		return hashes, ts, nil
	}
}

// SubscribeNewTxs subscribes to new transactions, and sends transactions on the given
// channel according to the filter. This function blocks and should be called in a goroutine.
// If there's an error receiving the new message it will close the channel and return the error.
func (c *Client) SubscribeNewTxs(filter *filter.Filter, ch chan<- *Transaction) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "x-api-key", c.key)

	protoFilter := &api.TxFilter{}
	if filter != nil {
		protoFilter.Encoded = filter.Encode()
	}

	res, err := c.client.SubscribeNewTxs(ctx, protoFilter)
	if err != nil {
		return fmt.Errorf("subscribing to transactions: %w", err)
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

func (c *Client) SubscribeNewExecutionPayloadHeaders(ch chan<- *ExecutionPayloadHeader) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "x-api-key", c.key)

	res, err := c.client.SubscribeExecutionHeaders(ctx, &emptypb.Empty{})
	if err != nil {
		return fmt.Errorf("subscribing to blocks: %w", err)
	}

	for {
		proto, err := res.Recv()
		if err != nil {
			close(ch)
			return err
		}

		ch <- ProtoToHeader(proto)
	}
}

func (c *Client) SubscribeNewExecutionPayloads(ch chan<- *ExecutionPayload) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "x-api-key", c.key)

	res, err := c.client.SubscribeExecutionPayloads(ctx, &emptypb.Empty{})
	if err != nil {
		return fmt.Errorf("subscribing to blocks: %w", err)
	}

	for {
		proto, err := res.Recv()
		if err != nil {
			close(ch)
			return err
		}

		ch <- ProtoToBlock(proto)
	}
}

func (c *Client) SubscribeNewBeaconBlocks(ch chan<- *BeaconBlock) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "x-api-key", c.key)

	res, err := c.client.SubscribeBeaconBlocks(ctx, &emptypb.Empty{})
	if err != nil {
		return fmt.Errorf("subscribing to blocks: %w", err)
	}

	for {
		proto, err := res.Recv()
		if err != nil {
			close(ch)
			return err
		}

		ch <- ProtoToBeaconBlock(proto)
	}
}
