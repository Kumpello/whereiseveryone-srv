package me

import (
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

type timestamp time.Time

func newTimestamp(t time.Time) timestamp {
	return timestamp(t.UTC())
}

func newTimestampPtr(t *time.Time) *timestamp {
	if t == nil {
		return nil
	}

	value := newTimestamp(*t)
	return &value
}

func (t timestamp) Time() time.Time {
	return time.Time(t)
}

func (t timestamp) Equal(u time.Time) bool {
	return t.Time().Equal(u)
}

func (t timestamp) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatInt(t.Time().UnixMilli(), 10)), nil
}

func (t *timestamp) UnmarshalJSON(data []byte) error {
	var value *int64
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	if value == nil {
		return errors.New("timestamp must be an integer in Unix milliseconds")
	}

	*t = newTimestamp(time.UnixMilli(*value))
	return nil
}

type friendState string

const (
	friendStateAccepted        friendState = "accepted"
	friendStatePendingIncoming friendState = "pending_incoming"
	friendStatePendingOutgoing friendState = "pending_outgoing"
)

type updateStatusRequest struct {
	// Status is at most 1024 characters; an empty string clears the status.
	Status string `json:"status" validate:"max=1024"`
}

type getFriendsResponse []friendDetails

type friendDetails struct {
	Username string `json:"username"`
	// Status is present only for accepted friends, even when empty.
	Status      *string          `json:"status,omitempty"`
	State       friendState      `json:"state"`
	Location    *locationDetails `json:"location,omitempty"`
	FriendSince *timestamp       `json:"friend_since"`
}

func newFriendDetails(username, status string, state friendState, friendSince *time.Time) friendDetails {
	friend := friendDetails{
		Username:    username,
		State:       state,
		FriendSince: newTimestampPtr(friendSince),
	}
	if state == friendStateAccepted {
		friend.Status = &status
	}
	return friend
}

type locationDetails struct {
	Longitude float64 `json:"longitude"`
	Latitude  float64 `json:"latitude"`
	Altitude  float64 `json:"altitude,omitempty"`
	Bearing   float64 `json:"bearing,omitempty"`
	Accuracy  float64 `json:"accuracy,omitempty"`
	Speed     float64 `json:"speed,omitempty"`

	// LastUpdate is the location fix time in Unix milliseconds (UTC).
	LastUpdate timestamp `json:"last_update" swaggertype:"integer" format:"int64" binding:"required"`
}

type updateLocationRequest struct {
	locationDetails `json:",inline"`
}

const (
	// Allow delayed uploads without accepting arbitrary historical dates.
	maxLocationAge = 24 * time.Hour
	// Device clocks may be slightly ahead of the server clock.
	maxLocationClockSkew = 5 * time.Minute
)

func (r updateLocationRequest) validatedLastUpdate(now time.Time) (time.Time, error) {
	now = now.UTC().Truncate(time.Millisecond)
	lastUpdate := r.LastUpdate.Time()
	if lastUpdate.IsZero() || lastUpdate.UnixMilli() <= 0 ||
		lastUpdate.Before(now.Add(-maxLocationAge)) || lastUpdate.After(now.Add(maxLocationClockSkew)) {
		return time.Time{}, errors.New("last_update must be positive Unix milliseconds within the location upload window")
	}
	// An accepted clock skew must not store a future date or block subsequent fixes.
	if lastUpdate.After(now) {
		return now.UTC().Truncate(time.Millisecond), nil
	}
	return lastUpdate, nil
}

type friendRequest struct {
	// Username is at most 64 characters.
	Username string `json:"username" validate:"max=64"`
}

type getPausedResponse []pausedFriendDetails

type pausedFriendDetails struct {
	Username string `json:"username"`
}
