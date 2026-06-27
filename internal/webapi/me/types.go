package me

import "time"

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
	FriendSince *time.Time       `json:"friend_since"`
}

func newFriendDetails(username, status string, state friendState, friendSince *time.Time) friendDetails {
	return friendDetails{
		Username:    username,
		Status:      status,
		State:       state,
		FriendSince: friendSince,
	}
}

type locationDetails struct {
	Longitude float64 `json:"longitude"`
	Latitude  float64 `json:"latitude"`
	Altitude  float64 `json:"altitude,omitempty"`
	Bearing   float64 `json:"bearing,omitempty"`
	Accuracy  float64 `json:"accuracy,omitempty"`

	// LastUpdate in UTC time
	LastUpdate time.Time `json:"last_update"`
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
