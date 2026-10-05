// Package me exposes authenticated profile and relationship endpoints.
package me

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/labstack/echo/v5"

	"whereiseveryone/internal/users"
	"whereiseveryone/internal/webapi/jsonerr"
	"whereiseveryone/pkg/id"
)

type friendCursor struct {
	Version int                   `json:"v"`
	Viewer  string                `json:"u"`
	State   users.FriendListState `json:"s"`
	After   string                `json:"a"`
}

func parseFriendPage(c *echo.Context, viewer id.ID) (users.FriendPageQuery, error) {
	query := users.FriendPageQuery{State: users.FriendListAccepted, Limit: users.MaxFriendPageSize}
	if state := c.QueryParam("state"); state != "" {
		query.State = users.FriendListState(state)
	}
	switch query.State {
	case users.FriendListAccepted, users.FriendListIncoming, users.FriendListOutgoing:
	default:
		return query, errors.New("invalid friend list state")
	}
	if limit := c.QueryParam("limit"); limit != "" {
		value, err := strconv.Atoi(limit)
		if err != nil || value < 1 || value > users.MaxFriendPageSize {
			return query, errors.New("friend page size must be between 1 and 50")
		}
		query.Limit = value
	}
	if token := c.QueryParam("cursor"); token != "" {
		if len(token) > 256 {
			return query, errors.New("invalid friend page cursor")
		}
		data, err := base64.RawURLEncoding.Strict().DecodeString(token)
		if err != nil {
			return query, fmt.Errorf("decode friend page cursor: %w", err)
		}
		var cursor friendCursor
		if decodeErr := json.Unmarshal(data, &cursor); decodeErr != nil {
			return query, fmt.Errorf("parse friend page cursor: %w", decodeErr)
		}
		if cursor.Version != 1 || cursor.Viewer != viewer.Hex() || cursor.State != query.State {
			return query, errors.New("friend page cursor belongs to a different viewer or list")
		}
		query.After, err = id.FromString(cursor.After)
		if err != nil || query.After.IsZero() {
			return query, errors.New("invalid friend page position")
		}
	}
	return query, nil
}

//nolint:nilnil // A nil cursor explicitly marks the final page, not an error.
func nextFriendCursor(viewer id.ID, state users.FriendListState, after id.ID) (*string, error) {
	if after.IsZero() {
		return nil, nil
	}
	data, err := json.Marshal(friendCursor{Version: 1, Viewer: viewer.Hex(), State: state, After: after.Hex()})
	if err != nil {
		return nil, fmt.Errorf("encode friend page cursor: %w", err)
	}
	value := base64.RawURLEncoding.EncodeToString(data)
	return &value, nil
}

func relationshipError(c *echo.Context, err error) error {
	response := jsonerr.EchoInternalError(err)
	switch {
	case errors.Is(err, users.ErrFriendsLimit), errors.Is(err, users.ErrIncomingRequestsLimit),
		errors.Is(err, users.ErrOutgoingRequestsLimit):
		response = jsonerr.EchoConflictError(err)
	case errors.Is(err, users.ErrAlreadyFriends):
		response = jsonerr.EchoInvalidRequestError(err)
	case errors.Is(err, users.ErrFriendRequestNotExists), errors.Is(err, users.ErrUserNotExists):
		response = jsonerr.EchoNotFoundError(err)
	}
	if writeErr := response.Echo(c); writeErr != nil {
		return fmt.Errorf("write relationship error: %w", writeErr)
	}
	return nil
}
