package eventsv1

import (
	gwpb "github.com/grpc-ecosystem/grpc-gateway/v2/proto/gateway"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/feed"
	blockfeed "github.com/prysmaticlabs/prysm/beacon-chain/core/feed/block"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/feed/operation"
	statefeed "github.com/prysmaticlabs/prysm/beacon-chain/core/feed/state"
	ethpb "github.com/prysmaticlabs/prysm/proto/eth/v1"
	"github.com/prysmaticlabs/prysm/proto/migration"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

const (
	headTopic                = "head"
	blockTopic               = "block"
	attestationTopic         = "attestation"
	voluntaryExitTopic       = "voluntary_exit"
	finalizedCheckpointTopic = "finalized_checkpoint"
	chainReorgTopic          = "chain_reorg"
)

var casesHandled = map[string]bool{
	headTopic:                true,
	blockTopic:               true,
	attestationTopic:         true,
	voluntaryExitTopic:       true,
	finalizedCheckpointTopic: true,
	chainReorgTopic:          true,
}

// StreamEvents allows requesting all events from a set of topics defined in the eth2.0-apis standard.
// The topics supported include block events, attestations, chain reorgs, voluntary exits,
// chain finality, and more.
func (s *Server) StreamEvents(
	req *ethpb.StreamEventsRequest, stream ethpb.Events_StreamEventsServer,
) error {
	if req == nil || len(req.Topics) == 0 {
		return status.Error(codes.InvalidArgument, "no topics specified to subscribe to")
	}
	// Check if the topics in the request are valid.
	requestedTopics := make(map[string]bool)
	for _, topic := range req.Topics {
		if _, ok := casesHandled[topic]; !ok {
			return status.Errorf(codes.InvalidArgument, "topic %s not allowed for event subscriptions", topic)
		}
		requestedTopics[topic] = true
	}

	// Subscribe to event feeds from information received in the beacon node runtime.
	blockChan := make(chan *feed.Event, 1)
	blockSub := s.BlockNotifier.BlockFeed().Subscribe(blockChan)

	opsChan := make(chan *feed.Event, 1)
	opsSub := s.OperationNotifier.OperationFeed().Subscribe(opsChan)

	stateChan := make(chan *feed.Event, 1)
	stateSub := s.StateNotifier.StateFeed().Subscribe(stateChan)

	defer blockSub.Unsubscribe()
	defer opsSub.Unsubscribe()
	defer stateSub.Unsubscribe()

	// Handle each event received and context cancelation.
	for {
		select {
		case event := <-blockChan:
			if err := s.handleBlockEvents(stream, requestedTopics, event); err != nil {
				return status.Errorf(codes.Internal, "could not handle block event: %v", err)
			}
		case event := <-opsChan:
			if err := s.handleBlockOperationEvents(stream, requestedTopics, event); err != nil {
				return status.Errorf(codes.Internal, "could not handle block operations event: %v", err)
			}
		case event := <-stateChan:
			if err := s.handleStateEvents(stream, requestedTopics, event); err != nil {
				return status.Errorf(codes.Internal, "could not handle state event: %v", err)
			}
		case <-s.Ctx.Done():
			return status.Errorf(codes.Canceled, "context canceled")
		case <-stream.Context().Done():
			return status.Errorf(codes.Canceled, "context canceled")
		}
	}
}

func (s *Server) handleBlockEvents(
	stream ethpb.Events_StreamEventsServer, requestedTopics map[string]bool, event *feed.Event,
) error {
	switch event.Type {
	case blockfeed.ReceivedBlock:
		if _, ok := requestedTopics[blockTopic]; !ok {
			return nil
		}
		blkData, ok := event.Data.(*blockfeed.ReceivedBlockData)
		if !ok {
			return nil
		}
		v1Data, err := migration.BlockIfaceToV1BlockHeader(blkData.SignedBlock)
		if err != nil {
			return err
		}
		item, err := v1Data.HashTreeRoot()
		if err != nil {
			return status.Errorf(codes.Internal, "could not hash tree root block %v", err)
		}
		eventBlock := &ethpb.EventBlock{
			Slot:  v1Data.Message.Slot,
			Block: item[:],
		}
		return s.streamData(stream, blockTopic, eventBlock)
	default:
		return nil
	}
}

func (s *Server) handleBlockOperationEvents(
	stream ethpb.Events_StreamEventsServer, requestedTopics map[string]bool, event *feed.Event,
) error {
	switch event.Type {
	case operation.AggregatedAttReceived:
		if _, ok := requestedTopics[attestationTopic]; !ok {
			return nil
		}
		attData, ok := event.Data.(*operation.AggregatedAttReceivedData)
		if !ok {
			return nil
		}
		v1Data := migration.V1Alpha1AggregateAttAndProofToV1(attData.Attestation)
		return s.streamData(stream, attestationTopic, v1Data)
	case operation.UnaggregatedAttReceived:
		if _, ok := requestedTopics[attestationTopic]; !ok {
			return nil
		}
		attData, ok := event.Data.(*operation.UnAggregatedAttReceivedData)
		if !ok {
			return nil
		}
		v1Data := migration.V1Alpha1AttestationToV1(attData.Attestation)
		return s.streamData(stream, attestationTopic, v1Data)
	case operation.ExitReceived:
		if _, ok := requestedTopics[voluntaryExitTopic]; !ok {
			return nil
		}
		exitData, ok := event.Data.(*operation.ExitReceivedData)
		if !ok {
			return nil
		}
		v1Data := migration.V1Alpha1ExitToV1(exitData.Exit)
		return s.streamData(stream, voluntaryExitTopic, v1Data)
	default:
		return nil
	}
}

func (s *Server) handleStateEvents(
	stream ethpb.Events_StreamEventsServer, requestedTopics map[string]bool, event *feed.Event,
) error {
	switch event.Type {
	case statefeed.NewHead:
		if _, ok := requestedTopics[headTopic]; !ok {
			return nil
		}
		head, ok := event.Data.(*ethpb.EventHead)
		if !ok {
			return nil
		}
		return s.streamData(stream, headTopic, head)
	case statefeed.FinalizedCheckpoint:
		if _, ok := requestedTopics[finalizedCheckpointTopic]; !ok {
			return nil
		}
		finalizedCheckpoint, ok := event.Data.(*ethpb.EventFinalizedCheckpoint)
		if !ok {
			return nil
		}
		return s.streamData(stream, finalizedCheckpointTopic, finalizedCheckpoint)
	case statefeed.Reorg:
		if _, ok := requestedTopics[chainReorgTopic]; !ok {
			return nil
		}
		reorg, ok := event.Data.(*ethpb.EventChainReorg)
		if !ok {
			return nil
		}
		return s.streamData(stream, chainReorgTopic, reorg)
	default:
		return nil
	}
}

func (s *Server) streamData(stream ethpb.Events_StreamEventsServer, name string, data proto.Message) error {
	returnData, err := anypb.New(data)
	if err != nil {
		return err
	}
	return stream.Send(&gwpb.EventSource{
		Event: name,
		Data:  returnData,
	})
}
