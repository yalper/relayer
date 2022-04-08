package relayer

import (
	"context"

	"github.com/cosmos/relayer/v2/relayer/provider"
	"github.com/cosmos/relayer/v2/relayer/provider/cosmos"
	"go.uber.org/multierr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// RelayMsgs contains the msgs that need to be sent to both a src and dst chain
// after a given relay round. MaxTxSize and MaxMsgLength are ignored if they are
// set to zero.
type RelayMsgs struct {
	Src          []provider.RelayerMessage `json:"src"`
	Dst          []provider.RelayerMessage `json:"dst"`
	MaxTxSize    uint64                    `json:"max_tx_size"`    // maximum permitted size of the msgs in a bundled relay transaction
	MaxMsgLength uint64                    `json:"max_msg_length"` // maximum amount of messages in a bundled relay transaction
}

// NewRelayMsgs returns an initialized version of relay messages
func NewRelayMsgs() *RelayMsgs {
	return &RelayMsgs{Src: []provider.RelayerMessage{}, Dst: []provider.RelayerMessage{}}
}

// Ready returns true if there are messages to relay
func (r *RelayMsgs) Ready() bool {
	if r == nil {
		return false
	}

	if len(r.Src) == 0 && len(r.Dst) == 0 {
		return false
	}
	return true
}

func (r *RelayMsgs) IsMaxTx(msgLen, txSize uint64) bool {
	return (r.MaxMsgLength != 0 && msgLen > r.MaxMsgLength) ||
		(r.MaxTxSize != 0 && txSize > r.MaxTxSize)
}

func EncodeMsgs(c *Chain, msgs []provider.RelayerMessage) []string {
	outMsgs := make([]string, 0, len(msgs))
	for _, msg := range msgs {
		bz, err := c.Encoding.Amino.MarshalJSON(msg)
		if err != nil {
			msgField := zap.Skip()
			if cm, ok := msg.(cosmos.CosmosMessage); ok {
				msgField = zap.Object("msg", cm)
			}
			c.log.Warn(
				"Failed to marshal message to amino JSON",
				msgField,
				zap.Error(err),
			)
		} else {
			outMsgs = append(outMsgs, string(bz))
		}
	}
	return outMsgs
}

func DecodeMsgs(c *Chain, msgs []string) []provider.RelayerMessage {
	outMsgs := make([]provider.RelayerMessage, 0, len(msgs))
	for _, msg := range msgs {
		var sm provider.RelayerMessage
		err := c.Encoding.Amino.UnmarshalJSON([]byte(msg), &sm)
		if err != nil {
			c.log.Warn(
				"Failed to unmarshal amino JSON message",
				zap.Binary("msg", []byte(msg)), // Although presented as a string, this is a binary blob.
				zap.Error(err),
			)
		} else {
			outMsgs = append(outMsgs, sm)
		}
	}
	return outMsgs
}

// RelayMsgSender is a narrow subset of a Chain,
// to simplify testing methods on RelayMsgs.
type RelayMsgSender struct {
	ChainID string

	// SendMessages is a function matching the signature of the same method
	// on the ChainProvider interface.
	//
	// Accepting this narrow subset of the interface greatly simplifies testing.
	SendMessages func(context.Context, []provider.RelayerMessage) (*provider.RelayerTxResponse, bool, error)
}

// AsRelayMsgSender converts c to a RelayMsgSender.
func AsRelayMsgSender(c *Chain) RelayMsgSender {
	return RelayMsgSender{
		ChainID:      c.ChainID(),
		SendMessages: c.ChainProvider.SendMessages,
	}
}

// SendMsgsResult is returned by (*RelayMsgs).Send.
// It contains details about the distinct results
// of sending messages to the corresponding chains.
type SendMsgsResult struct {
	// Count of successfully sent batches,
	// where "successful" means there was no error in sending the batch across the network,
	// and the remote end sent a response indicating success.
	SuccessfulSrcBatches, SuccessfulDstBatches int

	// Accumulation of errors encountered when sending to source or destination.
	// If multiple errors occurred, these will be multierr errors
	// which are displayed nicely through zap logging.
	SrcSendError, DstSendError error
}

// PartiallySent reports the presence of both some successfully sent batches
// and some errors.
func (r SendMsgsResult) PartiallySent() bool {
	return (r.SuccessfulSrcBatches > 0 || r.SuccessfulDstBatches > 0) &&
		(r.SrcSendError != nil || r.DstSendError != nil)
}

// Error returns any accumulated erors that occurred while sending messages.
func (r SendMsgsResult) Error() error {
	return multierr.Append(r.SrcSendError, r.DstSendError)
}

// MarshalLogObject satisfies the zapcore.ObjectMarshaler interface
// so that you can use zap.Object("send_result", r) when logging.
// This is typically useful when logging details about a partially sent result.
func (r SendMsgsResult) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddInt("successful_src_batches", r.SuccessfulSrcBatches)
	enc.AddInt("successful_dst_batches", r.SuccessfulDstBatches)
	if r.SrcSendError == nil {
		enc.AddString("src_send_errors", "<nil>")
	} else {
		enc.AddString("src_send_errors", r.SrcSendError.Error())
	}
	if r.DstSendError == nil {
		enc.AddString("dst_send_errors", "<nil>")
	} else {
		enc.AddString("dst_send_errors", r.DstSendError.Error())
	}

	return nil
}

func (r *RelayMsgs) Send(ctx context.Context, log *zap.Logger, src, dst RelayMsgSender) SendMsgsResult {
	//nolint:prealloc // can not be pre allocated
	var (
		msgLen, txSize uint64
		msgs           []provider.RelayerMessage

		result SendMsgsResult
	)

	// submit batches of relay transactions
	log.Info("Sending Src")
	for _, msg := range r.Src {
		if msg != nil {
			bz, err := msg.MsgBytes()
			if err != nil {
				panic(err)
			}

			msgLen++
			txSize += uint64(len(bz))

			if r.IsMaxTx(msgLen, txSize) {
				// Submit the transactions to src chain and update its status
				resp, success, err := src.SendMessages(ctx, msgs)
				if err != nil {
					logFailedTx(log, src.ChainID, resp, err, msgs)
					multierr.AppendInto(&result.SrcSendError, err)
				}
				if success {
					result.SuccessfulSrcBatches++
				}

				// clear the current batch and reset variables
				msgLen, txSize = 1, uint64(len(bz))
				msgs = []provider.RelayerMessage{}
			}
			msgs = append(msgs, msg)
		}
	}

	// submit leftover msgs
	if len(msgs) > 0 {
		resp, success, err := src.SendMessages(ctx, msgs)
		if err != nil {
			logFailedTx(log, src.ChainID, resp, err, msgs)
			multierr.AppendInto(&result.SrcSendError, err)
		}
		if success {
			result.SuccessfulSrcBatches++
		}
	}

	// reset variables

	log.Info("Sending Dst")
	msgLen, txSize = 0, 0
	msgs = []provider.RelayerMessage{}

	for _, msg := range r.Dst {
		if msg != nil {
			bz, err := msg.MsgBytes()
			if err != nil {
				panic(err)
			}

			msgLen++
			txSize += uint64(len(bz))

			if r.IsMaxTx(msgLen, txSize) {
				// Submit the transaction to dst chain and update its status
				log.Info("Before sending dst msgs")
				resp, success, err := dst.SendMessages(ctx, msgs)
				if err != nil {
					logFailedTx(log, dst.ChainID, resp, err, msgs)
					multierr.AppendInto(&result.DstSendError, err)
				}
				if success {
					result.SuccessfulDstBatches++
				}
				log.Info("AFter sending dst msgs")

				// clear the current batch and reset variables
				msgLen, txSize = 1, uint64(len(bz))
				msgs = []provider.RelayerMessage{}
			}
			msgs = append(msgs, msg)
		}
	}

	// submit leftover msgs
	if len(msgs) > 0 {
		resp, success, err := dst.SendMessages(ctx, msgs)
		if err != nil {
			logFailedTx(log, dst.ChainID, resp, err, msgs)
			multierr.AppendInto(&result.DstSendError, err)
		}
		if success {
			result.SuccessfulDstBatches++
		}
	}

	return result
}
