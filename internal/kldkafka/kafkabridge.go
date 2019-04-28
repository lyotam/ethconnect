// Copyright 2018 Kaleido, a ConsenSys business

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at

//     http://www.apache.org/licenses/LICENSE-2.0

// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kldkafka

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/Shopify/sarama"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/lyotam/ethconnect-quorum/internal/kldmessages"
	"github.com/lyotam/ethconnect-quorum/internal/kldutils"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

// KafkaBridgeConf defines the YAML config structure for a webhooks bridge instance
type KafkaBridgeConf struct {
	Kafka         KafkaCommonConf `json:"kafka"`
	MaxInFlight   int             `json:"maxInFlight"`
	MaxTXWaitTime int             `json:"maxTXWaitTime"`
	PredictNonces bool            `json:"alwaysManageNonce"`
	RPC           struct {
		URL string `json:"url"`
	} `json:"rpc"`
}

// KafkaBridge receives messages from Kafka and dispatches them to go-ethereum over JSON/RPC
type KafkaBridge struct {
	printYAML    *bool
	conf         KafkaBridgeConf
	kafka        KafkaCommon
	rpc          *rpc.Client
	processor    MsgProcessor
	inFlight     map[string]*msgContext
	inFlightCond *sync.Cond
}

// Conf gets the config for this bridge
func (k *KafkaBridge) Conf() *KafkaBridgeConf {
	return &k.conf
}

// SetConf sets the config for this bridge
func (k *KafkaBridge) SetConf(conf *KafkaBridgeConf) {
	k.conf = *conf
}

// ValidateConf validates the configuration
func (k *KafkaBridge) ValidateConf() (err error) {
	if k.conf.RPC.URL == "" {
		return fmt.Errorf("No JSON/RPC URL set for ethereum node")
	}
	if k.conf.MaxTXWaitTime < 10 {
		if k.conf.MaxTXWaitTime > 0 {
			log.Warnf("Maximum wait time increased from %d to minimum of 10 seconds", k.conf.MaxTXWaitTime)
		}
		k.conf.MaxTXWaitTime = 10
	}
	if k.conf.MaxInFlight == 0 {
		k.conf.MaxInFlight = 10
	}
	return
}

// CobraInit retruns a cobra command to configure this KafkaBridge
func (k *KafkaBridge) CobraInit() (cmd *cobra.Command) {
	cmd = &cobra.Command{
		Use:   "kafka",
		Short: "Kafka->Ethereum (JSON/RPC) Bridge",
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			log.Infof("Starting Kafka bridge")
			err = k.Start()
			return
		},
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
			if err = k.kafka.ValidateConf(); err != nil {
				return
			}
			err = k.ValidateConf()
			return
		},
	}
	k.kafka.CobraInit(cmd)
	cmd.Flags().IntVarP(&k.conf.MaxInFlight, "maxinflight", "m", kldutils.DefInt("KAFKA_MAX_INFLIGHT", 0), "Maximum messages to hold in-flight")
	cmd.Flags().StringVarP(&k.conf.RPC.URL, "rpc-url", "r", os.Getenv("ETH_RPC_URL"), "JSON/RPC URL for Ethereum node")
	cmd.Flags().IntVarP(&k.conf.MaxTXWaitTime, "tx-timeout", "x", kldutils.DefInt("ETH_TX_TIMEOUT", 0), "Maximum wait time for an individual transaction (seconds)")
	cmd.Flags().BoolVarP(&k.conf.PredictNonces, "predict-nonces", "P", false, "Predict the next nonce before sending (default=false for node-signed txns)")
	return
}

// MsgContext is passed for each message that arrives at the bridge
type MsgContext interface {
	// Get the headers of the message
	Headers() *kldmessages.CommonHeaders
	// Unmarshal the supplied message into a give type
	Unmarshal(msg interface{}) error
	// Send an error reply
	SendErrorReply(status int, err error)
	// Send an error reply
	SendErrorReplyWithTX(status int, err error, txHash string)
	// Send a reply that can be marshaled into bytes.
	// Sets all the common headers on behalf of the caller, based on the request context
	Reply(replyMsg kldmessages.ReplyWithHeaders)
	// Get a string summary
	String() string
}

type msgContext struct {
	timeReceived   time.Time
	producer       KafkaProducer
	requestCommon  kldmessages.RequestCommon
	reqOffset      string
	saramaMsg      *sarama.ConsumerMessage
	key            string
	bridge         *KafkaBridge
	complete       bool
	replyType      string
	replyTime      time.Time
	replyBytes     []byte
	replyPartition int32
	replyOffset    int64
}

// addInflightMsg creates a msgContext wrapper around a message with all the
// relevant context, and adds it to the inFlight map
// * Caller holds the inFlightCond mutex, and has already checked for capacity *
func (k *KafkaBridge) addInflightMsg(msg *sarama.ConsumerMessage, producer KafkaProducer) (pCtx *msgContext, err error) {
	ctx := msgContext{
		timeReceived: time.Now(),
		reqOffset:    fmt.Sprintf("%s:%d:%d", msg.Topic, msg.Partition, msg.Offset),
		saramaMsg:    msg,
		bridge:       k,
		producer:     producer,
	}
	// If the mesage is already in our inflight map, we've got a redelivery from Kafka.
	// We ignore it, as we'll already do the ack.
	var alreadyInflight bool
	if pCtx, alreadyInflight = k.inFlight[ctx.reqOffset]; alreadyInflight {
		log.Infof("Message already in-flight: %s", pCtx)
		// Return nil to idicate to caller not to duplicate process
		return nil, nil
	}

	// Add it to our inflight map - from this point on we need to ensure we remove it, to avoid leaks.
	// Messages are only removed from the inflight map when a response is sent, so it
	// is very important that the consumer of the wrapped context object calls Reply
	pCtx = &ctx
	k.inFlight[ctx.reqOffset] = pCtx
	log.Infof("Message now in-flight: %s", pCtx)
	// Attempt to process the headers from the original message,
	// which could fail. In which case we still have a msgContext inflight
	// that needs Reply (and offset commit). So our caller must
	// send a generic error reply (after dropping the lock).
	if err = json.Unmarshal(msg.Value, &ctx.requestCommon); err != nil {
		log.Errorf("Failed to unmarshal message headers: %s - Message=%s", err, string(msg.Value))
		return
	}
	headers := &ctx.requestCommon.Headers
	if headers.ID == "" {
		headers.ID = kldutils.UUIDv4()
	}
	// Use the account as the partitioning key, or fallback to the ID, which we ensure is non-null
	if headers.Account != "" {
		ctx.key = headers.Account
	} else {
		ctx.key = headers.ID
	}
	return
}

type ctxByOffset []*msgContext

func (a ctxByOffset) Len() int {
	return len(a)
}
func (a ctxByOffset) Swap(i, j int) {
	a[i], a[j] = a[j], a[i]
}
func (a ctxByOffset) Less(i, j int) bool {
	return a[i].saramaMsg.Offset < a[j].saramaMsg.Offset
}

// Mark that a currently in-flight context is now ready.
// Looks at the other in-flight messages for the same partition, and works out if
// we can move the offset forwards.
// * Caller holds the inFlightCond mutex *
func (k *KafkaBridge) setInFlightComplete(ctx *msgContext, consumer KafkaConsumer) (err error) {

	// Build an offset sorted list of the inflight
	ctx.complete = true
	var completeInParition []*msgContext
	for _, inflight := range k.inFlight {
		if inflight.saramaMsg.Partition == ctx.saramaMsg.Partition {
			completeInParition = append(completeInParition, inflight)
		}
	}
	sort.Sort(ctxByOffset(completeInParition))

	// Go forwards until the first that isn't complete
	var readyToAck []*msgContext
	for i := 0; i < len(completeInParition); i++ {
		if completeInParition[i].complete {
			readyToAck = append(readyToAck, completeInParition[i])
		} else {
			break
		}
	}

	canMark := len(readyToAck) > 0
	log.Debugf("Ready=%d:%d CanMark=%t Infight=%d InflightSamePartition=%d ReadyToAck=%d",
		ctx.saramaMsg.Partition, ctx.saramaMsg.Offset, canMark,
		len(k.inFlight), len(completeInParition), len(readyToAck))
	if canMark {
		// Remove all the ready-to-acks from the in-flight list
		for i := 0; i < len(readyToAck); i++ {
			delete(k.inFlight, readyToAck[i].reqOffset)
		}
		// Update the offset
		highestOffset := readyToAck[len(readyToAck)-1].saramaMsg
		log.Infof("Marking offset %d:%d", highestOffset.Offset, highestOffset.Partition)
		consumer.MarkOffset(highestOffset, "")
	}

	return
}

func (c *msgContext) Headers() *kldmessages.CommonHeaders {
	return &c.requestCommon.Headers
}

func (c *msgContext) Unmarshal(msg interface{}) (err error) {
	if err = json.Unmarshal(c.saramaMsg.Value, msg); err != nil {
		log.Errorf("Failed to parse message: %s - Message=%s", err, string(c.saramaMsg.Value))
	}
	return
}

func (c *msgContext) SendErrorReply(status int, err error) {
	c.SendErrorReplyWithTX(status, err, "")
}

func (c *msgContext) SendErrorReplyWithTX(status int, err error, txHash string) {
	log.Warnf("Failed to process message %s: %s", c, err)
	errMsg := kldmessages.NewErrorReply(err, c.saramaMsg.Value)
	errMsg.TXHash = txHash
	c.Reply(errMsg)
}

func (c *msgContext) Reply(replyMessage kldmessages.ReplyWithHeaders) {

	replyHeaders := replyMessage.ReplyHeaders()
	c.replyType = replyHeaders.MsgType
	replyHeaders.ID = kldutils.UUIDv4()
	replyHeaders.Context = c.requestCommon.Headers.Context
	replyHeaders.ReqID = c.requestCommon.Headers.ID
	replyHeaders.ReqOffset = c.reqOffset
	replyHeaders.ReqOffset = c.reqOffset
	replyHeaders.Received = c.timeReceived.Format(time.RFC3339)
	c.replyTime = time.Now()
	replyHeaders.Elapsed = c.replyTime.Sub(c.timeReceived).Seconds()
	c.replyBytes, _ = json.Marshal(replyMessage)
	log.Infof("Sending reply: %s", c)
	c.producer.Input() <- &sarama.ProducerMessage{
		Topic:    c.bridge.kafka.Conf().TopicOut,
		Key:      sarama.StringEncoder(c.key),
		Metadata: c.reqOffset,
		Value:    c,
	}
	return
}

func (c *msgContext) String() string {
	retval := fmt.Sprintf("MsgContext[%s:%s reqOffset=%s complete=%t received=%s",
		c.requestCommon.Headers.MsgType, c.requestCommon.Headers.ID,
		c.reqOffset, c.complete, c.timeReceived.Format(time.RFC3339))
	if c.replyType != "" {
		retval += fmt.Sprintf(" replied=%s replyType=%s",
			c.replyTime.Format(time.RFC3339), c.replyType)
	}
	retval += "]"
	return retval
}

// Length Gets the encoded length
func (c msgContext) Length() int {
	return len(c.replyBytes)
}

// Encode Does the encoding
func (c msgContext) Encode() ([]byte, error) {
	return c.replyBytes, nil
}

// NewKafkaBridge creates a new KafkaBridge
func NewKafkaBridge(printYAML *bool) *KafkaBridge {
	mp := newMsgProcessor()
	k := &KafkaBridge{
		printYAML:    printYAML,
		processor:    mp,
		inFlight:     make(map[string]*msgContext),
		inFlightCond: sync.NewCond(&sync.Mutex{}),
	}
	mp.conf = &k.conf // Inherit our configuration in the processor
	k.kafka = NewKafkaCommon(&SaramaKafkaFactory{}, &k.conf.Kafka, k)
	return k
}

// ConsumerMessagesLoop - goroutine to process messages
func (k *KafkaBridge) ConsumerMessagesLoop(consumer KafkaConsumer, producer KafkaProducer, wg *sync.WaitGroup) {
	log.Debugf("Kafka consumer loop started")
	for msg := range consumer.Messages() {
		k.inFlightCond.L.Lock()
		log.Infof("Kafka consumer received message: Partition=%d Offset=%d", msg.Partition, msg.Offset)

		// We cannot build up an infinite number of messages in memory
		for len(k.inFlight) >= k.conf.MaxInFlight {
			log.Infof("Too many messages in-flight: In-flight=%d Max=%d", len(k.inFlight), k.conf.MaxInFlight)
			k.inFlightCond.Wait()
		}
		// addInflightMsg always adds the message, even if it cannot
		// be parsed
		msgCtx, err := k.addInflightMsg(msg, producer)
		// Unlock before any further processing
		k.inFlightCond.L.Unlock()
		if msgCtx == nil {
			// This was a dup
		} else if err == nil {
			// Dispatch for processing if we parsed the message successfully
			k.processor.OnMessage(msgCtx)
		} else {
			// Dispatch a generic 'bad data' reply
			errMsg := kldmessages.NewErrorReply(err, msg.Value)
			msgCtx.Reply(errMsg)
		}
	}
	wg.Done()
}

// ProducerErrorLoop - goroutine to process producer errors
func (k *KafkaBridge) ProducerErrorLoop(consumer KafkaConsumer, producer KafkaProducer, wg *sync.WaitGroup) {
	log.Debugf("Kafka producer error loop started")
	defer wg.Done()
	for err := range producer.Errors() {
		k.inFlightCond.L.Lock()
		// If we fail to send a reply, this is significant. We have a request in flight
		// and we have probably already sent the message.
		// Currently we panic, on the basis that we will be restarted by Docker
		// to drive retry logic. In the future we might consider recreating the
		// producer and attempting to resend the message a number of times -
		// keeping a retry counter on the msgContext object
		reqOffset := err.Msg.Metadata.(string)
		ctx := k.inFlight[reqOffset]
		log.Errorf("Kafka producer failed for reply %s to reqOffset %s: %s", ctx, reqOffset, err)
		panic(err)
		// k.inFlightCond.L.Unlock() - unreachable while we have a panic
	}
}

// ProducerSuccessLoop - goroutine to process producer successes
func (k *KafkaBridge) ProducerSuccessLoop(consumer KafkaConsumer, producer KafkaProducer, wg *sync.WaitGroup) {
	log.Debugf("Kafka producer successes loop started")
	defer wg.Done()
	for msg := range producer.Successes() {
		k.inFlightCond.L.Lock()
		reqOffset := msg.Metadata.(string)
		if ctx, ok := k.inFlight[reqOffset]; ok {
			log.Infof("Reply sent: %s", ctx)
			// While still holding the lock, add this to the completed list
			k.setInFlightComplete(ctx, consumer)
			// We've reduced the in-flight count - wake any waiting consumer go func
			k.inFlightCond.Broadcast()
		} else {
			// This should never happen. Represents a logic bug that must be diagnosed.
			err := fmt.Errorf("Received confirmation for message not in in-flight map: %s", reqOffset)
			panic(err)
		}
		k.inFlightCond.L.Unlock()
	}
}

func (k *KafkaBridge) connect() (err error) {
	// Connect the client
	if k.rpc, err = rpc.Dial(k.conf.RPC.URL); err != nil {
		err = fmt.Errorf("JSON/RPC connection to %s failed: %s", k.conf.RPC.URL, err)
		return
	}
	k.processor.Init(k.rpc, k.conf.MaxTXWaitTime)
	log.Debug("JSON/RPC connected. URL=", k.conf.RPC.URL)

	return
}

// Start kicks off the bridge
func (k *KafkaBridge) Start() (err error) {

	if *k.printYAML {
		b, err := kldutils.MarshalToYAML(&k.conf)
		print("# YAML Configuration snippet for Kafka->Ethereum bridge\n" + string(b))
		return err
	}

	// Connect the RPC URL
	if err = k.connect(); err != nil {
		return
	}

	// Defer to KafkaCommon processing
	err = k.kafka.Start()
	return
}
