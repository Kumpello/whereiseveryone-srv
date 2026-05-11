package me

import (
	"errors"
	"net/http"
	"whereiseveryone/internal/users"
	"whereiseveryone/internal/webapi/binder"
	"whereiseveryone/internal/webapi/jsonerr"
	"whereiseveryone/pkg/timer"

	"github.com/labstack/echo/v4"
)

type mux struct {
	userAdapter users.Adapter
	timer       timer.Timer
}

func NewMux(userAdapter users.Adapter, timer timer.Timer) *mux {
	return &mux{userAdapter: userAdapter, timer: timer}
}

func (m *mux) Route(g *echo.Group, _ echo.MiddlewareFunc) {
	g.PUT("/status", m.updateStatus)
	g.GET("/friends", m.getFriends)
	g.PUT("/location", m.updateLocation)
	g.POST("/friend", m.befriend)
	g.DELETE("/friend", m.unfriend)
	g.POST("/friend/accept", m.acceptFriend)
	g.POST("/friend/reject", m.rejectFriend)
}

// updateStatus
//
// @summary update status
// @description updates logged user status (text status)
// @tags me
// @accept json
// @produce json
// @param status body updateStatusRequest true "update status object"
// @success 204
// @failure 400 {object} jsonerr.JSONError "invalid request"
// @failure 401 {object} jsonerr.JSONError "invalid token"
// @failure 500 {object} jsonerr.JSONError "internal server error"
// @router /me/status [PUT]
func (m *mux) updateStatus(c echo.Context) error {
	request, bindErr := binder.BindRequest[updateStatusRequest](c, true)
	if bindErr != nil {
		return bindErr.Echo(c)
	}
	defer request.Cancel()

	requestData := request.Request
	status := requestData.Status

	err := m.userAdapter.UpdateStatus(request.Context(), request.UserID(), status)
	if err != nil {
		return jsonerr.EchoInternalError(err).Echo(c)
	}

	return c.NoContent(204)
}

// getFriends
//
// @summary get friends details
// @description returns accepted friends and pending friend requests
// @tags me
// @produce json
// @success 200 {object} getFriendsResponse
// @failure 401 {object} jsonerr.JSONError "invalid token"
// @failure 500 {object} jsonerr.JSONError "internal server error"
// @router /me/friends [GET]
func (m *mux) getFriends(c echo.Context) error {
	request, bindErr := binder.BindRequest[binder.EmptyBody](c, true)
	if bindErr != nil {
		return bindErr.Echo(c)
	}
	defer request.Cancel()

	ctx := request.Context()

	user, err := m.userAdapter.GetUser(ctx, request.UserID())
	if err != nil {
		return jsonerr.EchoInternalError(err).Echo(c)
	}

	result := make(getFriendsResponse, 0)

	// Accepted friends
	friends, err := m.userAdapter.GetUsers(
		ctx,
		user.SubscribedUsers,
	)
	if err != nil {
		return jsonerr.EchoInternalError(err).Echo(c)
	}

	for _, u := range friends {
		friend := friendDetails{
			Username: u.Auth.Username,
			Status:   u.Status,
			State:    friendStateAccepted,
		}

		if u.Location != nil {
			friend.Location = locationDetails{
				Longitude:  u.Location.Longitude,
				Latitude:   u.Location.Latitude,
				Altitude:   u.Location.Altitude,
				Bearing:    u.Location.Bearing,
				Accuracy:   u.Location.Accuracy,
				LastUpdate: u.Location.LastUpdate,
			}
		}

		result = append(result, friend)
	}

	// Incoming pending requests
	incomingUsers, err := m.userAdapter.GetUsers(
		ctx,
		user.PendingIncomingFriendRequests,
	)
	if err != nil {
		return jsonerr.EchoInternalError(err).Echo(c)
	}

	for _, u := range incomingUsers {
		result = append(result, friendDetails{
			Username: u.Auth.Username,
			Status:   u.Status,
			State:    friendStatePendingIncoming,
		})
	}

	// Outgoing pending requests
	outgoingUsers, err := m.userAdapter.GetUsers(
		ctx,
		user.PendingOutgoingFriendRequests,
	)
	if err != nil {
		return jsonerr.EchoInternalError(err).Echo(c)
	}

	for _, u := range outgoingUsers {
		result = append(result, friendDetails{
			Username: u.Auth.Username,
			Status:   u.Status,
			State:    friendStatePendingOutgoing,
		})
	}

	return c.JSON(http.StatusOK, result)
}

// updateLocation
//
// @summary update location
// @description update logged user location
// @tags me
// @accept json
// @param location body updateLocationRequest true "update location object"
// @success 204
// @failure 400 {object} jsonerr.JSONError "invalid request"
// @failure 401 {object} jsonerr.JSONError "invalid token"
// @failure 500 {object} jsonerr.JSONError "internal server error"
// @router /me/location [PUT]
func (m *mux) updateLocation(c echo.Context) error {
	request, bindErr := binder.BindRequest[updateLocationRequest](c, true)
	if bindErr != nil {
		return bindErr.Echo(c)
	}
	defer request.Cancel()

	newLoc := request.Request
	err := m.userAdapter.UpdateLocation(request.Context(), request.UserID(), users.Location{
		Longitude:  newLoc.Longitude,
		Latitude:   newLoc.Latitude,
		Altitude:   newLoc.Altitude,
		Bearing:    newLoc.Bearing,
		Accuracy:   newLoc.Accuracy,
		LastUpdate: newLoc.LastUpdate,
	})
	if err != nil {
		return jsonerr.EchoInternalError(err).Echo(c)
	}

	return c.NoContent(204)
}

// befriend
//
// @summary send friend request
// @description sends friend request to another user
// @tags me
// @accept json
// @param user body friendRequest true "user to friend"
// @success 204
// @failure 400 {object} jsonerr.JSONError "invalid request"
// @failure 401 {object} jsonerr.JSONError "invalid token"
// @failure 404 {object} jsonerr.JSONError "requested user not exists"
// @failure 500 {object} jsonerr.JSONError "internal server error"
// @router /me/friend [POST]
func (m *mux) befriend(c echo.Context) error {
	request, bindErr := binder.BindRequest[friendRequest](c, true)
	if bindErr != nil {
		return bindErr.Echo(c)
	}
	defer request.Cancel()

	ctx := request.Context()
	currentUserID := request.UserID()

	userToBefriend, err := m.userAdapter.GetUserByUsername(
		ctx,
		request.Request.Username,
	)
	if err != nil {
		if errors.Is(err, users.ErrUserNotExists) {
			return jsonerr.EchoNotFoundError(err).Echo(c)
		}

		return jsonerr.EchoInternalError(err).Echo(c)
	}

	if userToBefriend.ID == currentUserID {
		return jsonerr.EchoInvalidRequestError(
			errors.New("cannot befriend yourself"),
		).Echo(c)
	}

	currentUser, err := m.userAdapter.GetUser(ctx, currentUserID)
	if err != nil {
		return jsonerr.EchoInternalError(err).Echo(c)
	}

	if currentUser.SubscribeUser(userToBefriend.ID) {
		return jsonerr.EchoInvalidRequestError(
			errors.New("user already befriended"),
		).Echo(c)
	}

	err = m.userAdapter.SendFriendRequest(
		ctx,
		currentUserID,
		userToBefriend.ID,
	)
	if err != nil {
		return jsonerr.EchoInternalError(err).Echo(c)
	}

	return c.NoContent(http.StatusNoContent)
}

// unfriend
//
// @summary remove friend
// @description removes friend and clears pending requests between users
// @tags me
// @accept json
// @param user body friendRequest true "user to unfriend"
// @success 204
// @failure 400 {object} jsonerr.JSONError "invalid request"
// @failure 401 {object} jsonerr.JSONError "invalid token"
// @failure 404 {object} jsonerr.JSONError "requested user not exists"
// @failure 500 {object} jsonerr.JSONError "internal server error"
// @router /me/friend [DELETE]
func (m *mux) unfriend(c echo.Context) error {
	request, bindErr := binder.BindRequest[friendRequest](c, true)
	if bindErr != nil {
		return bindErr.Echo(c)
	}
	defer request.Cancel()

	userToUnfriend, err := m.userAdapter.GetUserByUsername(request.Context(), request.Request.Username)
	if err != nil {
		if errors.Is(err, users.ErrUserNotExists) {
			return jsonerr.EchoNotFoundError(err).Echo(c)
		}
		return jsonerr.EchoInternalError(err).Echo(c)
	}

	err = m.userAdapter.UnfriendUser(request.Context(), request.UserID(), userToUnfriend.ID)
	if err != nil {
		return jsonerr.EchoInternalError(err).Echo(c)
	}

	return c.NoContent(204)
}

// acceptFriend
//
// @summary accept friend request
// @description accepts pending friend request from another user
// @tags me
// @accept json
// @param user body friendRequest true "user to accept"
// @success 204
// @failure 400 {object} jsonerr.JSONError "invalid request"
// @failure 401 {object} jsonerr.JSONError "invalid token"
// @failure 404 {object} jsonerr.JSONError "requested user not exists"
// @failure 500 {object} jsonerr.JSONError "internal server error"
// @router /me/friend/accept [POST]
func (m *mux) acceptFriend(c echo.Context) error {
	request, bindErr := binder.BindRequest[friendRequest](c, true)
	if bindErr != nil {
		return bindErr.Echo(c)
	}
	defer request.Cancel()

	ctx := request.Context()

	requester, err := m.userAdapter.GetUserByUsername(
		ctx,
		request.Request.Username,
	)
	if err != nil {
		if errors.Is(err, users.ErrUserNotExists) {
			return jsonerr.EchoNotFoundError(err).Echo(c)
		}

		return jsonerr.EchoInternalError(err).Echo(c)
	}

	err = m.userAdapter.AcceptFriendRequest(
		ctx,
		request.UserID(),
		requester.ID,
	)
	if err != nil {
		return jsonerr.EchoInternalError(err).Echo(c)
	}

	return c.NoContent(http.StatusNoContent)
}

// rejectFriend
//
// @summary reject friend request
// @description rejects pending friend request from another user
// @tags me
// @accept json
// @param user body friendRequest true "user to reject"
// @success 204
// @failure 400 {object} jsonerr.JSONError "invalid request"
// @failure 401 {object} jsonerr.JSONError "invalid token"
// @failure 404 {object} jsonerr.JSONError "requested user not exists"
// @failure 500 {object} jsonerr.JSONError "internal server error"
// @router /me/friend/reject [POST]
func (m *mux) rejectFriend(c echo.Context) error {
	request, bindErr := binder.BindRequest[friendRequest](c, true)
	if bindErr != nil {
		return bindErr.Echo(c)
	}
	defer request.Cancel()

	ctx := request.Context()

	requester, err := m.userAdapter.GetUserByUsername(
		ctx,
		request.Request.Username,
	)
	if err != nil {
		if errors.Is(err, users.ErrUserNotExists) {
			return jsonerr.EchoNotFoundError(err).Echo(c)
		}

		return jsonerr.EchoInternalError(err).Echo(c)
	}

	err = m.userAdapter.RejectFriendRequest(
		ctx,
		request.UserID(),
		requester.ID,
	)
	if err != nil {
		return jsonerr.EchoInternalError(err).Echo(c)
	}

	return c.NoContent(http.StatusNoContent)
}
