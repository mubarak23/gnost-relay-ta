package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)
type RequestPayload struct {
	ID        string         `json:"id"`
	PubKey    string         `json:"pubkey"`
	CreatedAt int64          `json:"created_at"`
	Kind      int            `json:"kind"`
	Ta        string         `json:"ta"`
	Amt       float64         `json:"amt"`
	Addr      string         `json:"addr"`
	Fee       float64         `json:"fee"`
	Content   string         `json:"content"`
	Sig       string         `json:"sig"`
}



func validateEventContent(payload RequestPayload) error {
	switch payload.Content {
	case "TAHUB_CREATE_USER", "TAHUB_GET_BALANCES":
		// These events are valid, no further checks needed
		if payload.Kind != 1 {
			return errors.New("Field 'Kind' must be 1")
		}
		return nil
	case "TAHUB_RECEIVE_ADDRESS_FOR_ASSET":
		// Validate specific fields for TAHUB_RECEIVE_ADDRESS_FOR_ASSET event
		if payload.Kind != 1 {
			return errors.New("Field 'Kind' must be 1")
		}

		if len(payload.Ta) == 0 || payload.Ta == "" {
			return errors.New("Field 'ta' must exist and not be empty")
		}
		if payload.Amt < 0 || payload.Amt != float64(int64(payload.Amt)) {
			return errors.New("Field 'amt' must be a positive integer (u64)")
		}
		return nil
	case "TAHUB_SEND_ASSET":
		// Validate specific fields for TAHUB_SEND_ASSET event
		if payload.Kind != 1 {
			return errors.New("Field 'Kind' must be 1")
		}

		if len(payload.Addr) == 0 || payload.Addr == "" {
			return errors.New("Field 'addr' must exist and not be empty")
		}
		if payload.Fee < 0 || payload.Fee != float64(int64(payload.Fee)) {
			return errors.New("Field 'fee' must be a positive integer (u64)")
		}
		return nil
	default:
		return errors.New("Invalid event content")
	}
}

func validateNoStrEventMiddleware(next func(http.ResponseWriter, *http.Request, RequestPayload) RequestPayload) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Upgrade the connection to WebSocket
		conn, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			http.Error(w, "Failed to upgrade to WebSocket", http.StatusBadRequest)
			return
		}
		defer conn.Close()

		// Read the payload from the WebSocket connection
		_, payload, err := wsutil.ReadClientData(conn)
		if err != nil {

			err := errors.New("Failed to read WebSocket data")
			fmt.Println(err)
			return
		}


		// Convert payload to string
		payloadStr := string(payload)

		var requestPayload RequestPayload
		decoder := json.NewDecoder(strings.NewReader(payloadStr))

		if err := decoder.Decode(&requestPayload); err != nil {
			
			err := errors.New("Invalid JSON payload")
			fmt.Println(err)
			return
		}

		// Perform validation based on the event content
		if err := validateEventContent(requestPayload); err != nil {
			
			fmt.Println(err)
			return
		}

		// If conditions are met, proceed to the next handler
		next(w, r, requestPayload)
	}
}




