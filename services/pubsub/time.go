package pubsub

import (
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
)

// timestampOf converts an injected-clock instant for the wire.
func timestampOf(t time.Time) *timestamppb.Timestamp { return timestamppb.New(t) }
