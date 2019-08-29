package stream

import (
	"fmt"

	"github.com/let-z-go/pbrpc/internal/protocol"
)

const (
	HangupErrorAborted                 = protocol.HANGUP_ERROR_ABORTED
	HangupErrorBadIncomingPacket       = protocol.HANGUP_ERROR_BAD_INCOMING_PACKET
	HangupErrorTooManyIncomingRequests = protocol.HANGUP_ERROR_TOO_MANY_INCOMING_REQUESTS
	HangupErrorOutgoingPacketTooLarge  = protocol.HANGUP_ERROR_OUTGOING_PACKET_TOO_LARGE
	HangupErrorSystem                  = protocol.HANGUP_ERROR_SYSTEM
)

type HangupError struct {
	Code      HangupErrorCode
	IsPassive bool
	ExtraData ExtraData
}

func (self *HangupError) Error() string {
	message := "pbrpc/stream: hangup: "

	switch self.Code {
	case HangupErrorAborted:
		message += "aborted"
	case HangupErrorBadIncomingPacket:
		message += "bad incoming packet"
	case HangupErrorTooManyIncomingRequests:
		message += "too many incoming requests"
	case HangupErrorOutgoingPacketTooLarge:
		message += "outgoing packet too large"
	case HangupErrorSystem:
		message += "system"
	default:
		message += fmt.Sprintf("error %d", self.Code)
	}

	if self.IsPassive {
		message += " (passive)"
	} else {
		message += " (active)"
	}

	return message
}

type HangupErrorCode = protocol.HangupErrorCode
type ExtraData = map[string][]byte
