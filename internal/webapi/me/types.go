package me

import (
	"encoding/json"
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
	var value int64
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}

	*t = newTimestamp(time.UnixMilli(value))
	return nil
}

type friendState string

const (
	friendStateAccepted        friendState = "accepted"
	friendStatePendingIncoming friendState = "pending_incoming"
	friendStatePendingOutgoing friendState = "pending_outgoing"
)

type updateStatusRequest struct {
	Status string `json:"status"`
}

type getFriendsResponse []friendDetails

type friendDetails struct {
	Username    string           `json:"username"`
	Status      string           `json:"status"`
	State       friendState      `json:"state"`
	Location    *locationDetails `json:"location,omitempty"`
	FriendSince *timestamp       `json:"friend_since"`
}

func newFriendDetails(username, status string, state friendState, friendSince *time.Time) friendDetails {
	return friendDetails{
		Username:    username,
		Status:      status,
		State:       state,
		FriendSince: newTimestampPtr(friendSince),
	}
}

type locationDetails struct {
	Longitude float64 `json:"longitude"`
	Latitude  float64 `json:"latitude"`
	Altitude  float64 `json:"altitude,omitempty"`
	Bearing   float64 `json:"bearing,omitempty"`
	Accuracy  float64 `json:"accuracy,omitempty"`
	Speed     float64 `json:"speed,omitempty"`

	// LastUpdate in UTC time
	LastUpdate timestamp `json:"last_update"`
}

type updateLocationRequest struct {
	locationDetails `json:",inline"`
}

type friendRequest struct {
	Username string `json:"username"`
}

type getPausedResponse []pausedFriendDetails

type pausedFriendDetails struct {
	Username string `json:"username"`
}
