package me

import (
	"errors"
	"fmt"
	"net/http"
	"whereiseveryone/internal/users"
	"whereiseveryone/internal/webapi"
	"whereiseveryone/internal/webapi/binder"
	"whereiseveryone/internal/webapi/jsonerr"
	"whereiseveryone/pkg/timer"

	"github.com/labstack/echo/v5"
)

type mux struct {
	userAdapter users.Adapter
	timer       timer.Timer
}

func NewMux(userAdapter users.Adapter, timer timer.Timer) *mux {
	return &mux{userAdapter: userAdapter, timer: timer}
}

func (m *mux) Route(g *echo.Group, _ echo.MiddlewareFunc) {
	g.PUT("/status", m.updateStatus, webapi.RequireJSON)
	g.GET("/friends", m.getFriends)
	g.PUT("/location", m.updateLocation, webapi.RequireJSON)
	g.DELETE("/location", m.wipeLocation)
	g.POST("/friend", m.befriend, webapi.RequireJSON)
	g.DELETE("/friend", m.unfriend, webapi.RequireJSON)
	g.POST("/friend/accept", m.acceptFriend, webapi.RequireJSON)
	g.POST("/friend/reject", m.rejectFriend, webapi.RequireJSON)
	g.POST("/sharing/stop", m.stopSharing, webapi.RequireJSON)
	g.POST("/sharing/resume", m.resumeSharing, webapi.RequireJSON)
	g.GET("/sharing", m.getPaused)
}

// updateStatus
//
// @summary update status
// @description updates logged user status (text status)
// @tags me
// @accept json
// @failure 413 {object} jsonerr.JSONError "request body exceeds 16 KiB"
// @failure 415 {object} jsonerr.JSONError "content type must be application/json"
// @produce json
// @param status body updateStatusRequest true "update status object"
// @success 204
// @failure 400 {object} jsonerr.JSONError "invalid request"
// @failure 401 {object} jsonerr.JSONError "invalid token"
// @failure 500 {object} jsonerr.JSONError "internal server error"
// @failure 503 {object} jsonerr.JSONError "server busy; retry after Retry-After seconds"
// @header 503 {string} Retry-After "Minimum delay in seconds before retrying (1)"
// @router /me/status [PUT]
func (m *mux) updateStatus(c *echo.Context) error {
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
// @description One bounded relationship page. Pending entries omit status and location.
// @tags me
// @produce json
// @param state query string false "list" Enums(accepted,pending_incoming,pending_outgoing) default(accepted)
// @param limit query int false "page size" minimum(1) maximum(50) default(50)
// @param cursor query string false "opaque next_cursor from the preceding page"
// @success 200 {object} getFriendsResponse
// @failure 400 {object} jsonerr.JSONError "invalid pagination parameters"
// @failure 401 {object} jsonerr.JSONError "invalid token"
// @failure 500 {object} jsonerr.JSONError "internal server error"
// @failure 503 {object} jsonerr.JSONError "server busy; retry after Retry-After seconds"
// @header 503 {string} Retry-After "Minimum delay in seconds before retrying (1)"
// @router /me/friends [GET]
func (m *mux) getFriends(c *echo.Context) error {
	request, bindErr := binder.BindRequest[binder.EmptyBody](c, true)
	if bindErr != nil {
		return bindErr.Echo(c)
	}
	defer request.Cancel()
	query, queryErr := parseFriendPage(c, request.UserID())
	if queryErr != nil {
		if err := jsonerr.EchoInvalidRequestError(queryErr).Echo(c); err != nil {
			return fmt.Errorf("write invalid friend page response: %w", err)
		}
		return nil
	}
	page, err := m.userAdapter.GetFriendPage(request.Context(), request.UserID(), query)
	if err != nil {
		return relationshipError(c, err)
	}
	next, err := nextFriendCursor(request.UserID(), query.State, page.NextID)
	if err != nil {
		return relationshipError(c, err)
	}
	result := getFriendsResponse{Items: make([]friendDetails, 0, len(page.Entries)), NextCursor: next}
	for _, entry := range page.Entries {
		u := entry.Peer
		friend := newFriendDetails(u.Username, u.Status, friendState(query.State), entry.FriendSince)
		if query.State == users.FriendListAccepted && u.Location != nil && u.LocationVisible {
			friend.Location = &locationDetails{
				Longitude: u.Location.Longitude, Latitude: u.Location.Latitude,
				Altitude: u.Location.Altitude, Bearing: u.Location.Bearing,
				Accuracy: u.Location.Accuracy, Speed: u.Location.Speed,
				LastUpdate: newTimestamp(u.Location.LastUpdate),
			}
		}
		result.Items = append(result.Items, friend)
	}
	return c.JSON(http.StatusOK, result)
}

// updateLocation
//
// @summary update location
// @description Unix-ms fix time: max age 24h, future skew 5m (capped); older/duplicate fixes ignored.
// @tags me
// @accept json
// @failure 413 {object} jsonerr.JSONError "request body exceeds 16 KiB"
// @failure 415 {object} jsonerr.JSONError "content type must be application/json"
// @param location body updateLocationRequest true "update location object"
// @success 204
// @failure 400 {object} jsonerr.JSONError "invalid request"
// @failure 401 {object} jsonerr.JSONError "invalid token"
// @failure 500 {object} jsonerr.JSONError "internal server error"
// @failure 503 {object} jsonerr.JSONError "server busy; retry after Retry-After seconds"
// @header 503 {string} Retry-After "Minimum delay in seconds before retrying (1)"
// @router /me/location [PUT]
func (m *mux) updateLocation(c *echo.Context) error {
	request, bindErr := binder.BindRequest[updateLocationRequest](c, true)
	if bindErr != nil {
		return bindErr.Echo(c)
	}
	defer request.Cancel()

	newLoc := request.Request
	lastUpdate, validationErr := newLoc.validatedLastUpdate(m.timer.Now())
	if validationErr != nil {
		if responseErr := jsonerr.EchoInvalidRequestError(validationErr).Echo(c); responseErr != nil {
			return fmt.Errorf("write invalid location response: %w", responseErr)
		}
		return nil
	}
	err := m.userAdapter.UpdateLocation(request.Context(), request.UserID(), users.Location{
		Longitude:  newLoc.Longitude,
		Latitude:   newLoc.Latitude,
		Altitude:   newLoc.Altitude,
		Bearing:    newLoc.Bearing,
		Accuracy:   newLoc.Accuracy,
		Speed:      newLoc.Speed,
		LastUpdate: lastUpdate,
	})
	if err != nil {
		return jsonerr.EchoInternalError(err).Echo(c)
	}

	return c.NoContent(204)
}

// wipeLocation
//
// @summary wipe location
// @description nullify logged user location
// @tags me
// @success 204
// @failure 401 {object} jsonerr.JSONError "invalid token"
// @failure 500 {object} jsonerr.JSONError "internal server error"
// @failure 503 {object} jsonerr.JSONError "server busy; retry after Retry-After seconds"
// @header 503 {string} Retry-After "Minimum delay in seconds before retrying (1)"
// @router /me/location [DELETE]
func (m *mux) wipeLocation(c *echo.Context) error {
	request, bindErr := binder.BindRequest[binder.EmptyBody](c, true)
	if bindErr != nil {
		return bindErr.Echo(c)
	}
	defer request.Cancel()

	err := m.userAdapter.WipeLocation(request.Context(), request.UserID())
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
// @failure 413 {object} jsonerr.JSONError "request body exceeds 16 KiB"
// @failure 415 {object} jsonerr.JSONError "content type must be application/json"
// @param user body friendRequest true "user to friend"
// @success 204
// @failure 400 {object} jsonerr.JSONError "invalid request"
// @failure 401 {object} jsonerr.JSONError "invalid token"
// @failure 404 {object} jsonerr.JSONError "requested user not exists"
// @failure 409 {object} jsonerr.JSONError "friend or pending request limit reached"
// @failure 500 {object} jsonerr.JSONError "internal server error"
// @failure 503 {object} jsonerr.JSONError "server busy; retry after Retry-After seconds"
// @header 503 {string} Retry-After "Minimum delay in seconds before retrying (1)"
// @router /me/friend [POST]
func (m *mux) befriend(c *echo.Context) error {
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
		return relationshipError(c, err)
	}

	return c.NoContent(http.StatusNoContent)
}

// unfriend
//
// @summary remove friend
// @description removes friend and clears pending requests between users
// @tags me
// @accept json
// @failure 413 {object} jsonerr.JSONError "request body exceeds 16 KiB"
// @failure 415 {object} jsonerr.JSONError "content type must be application/json"
// @param user body friendRequest true "user to unfriend"
// @success 204
// @failure 400 {object} jsonerr.JSONError "invalid request"
// @failure 401 {object} jsonerr.JSONError "invalid token"
// @failure 404 {object} jsonerr.JSONError "requested user not exists"
// @failure 500 {object} jsonerr.JSONError "internal server error"
// @failure 503 {object} jsonerr.JSONError "server busy; retry after Retry-After seconds"
// @header 503 {string} Retry-After "Minimum delay in seconds before retrying (1)"
// @router /me/friend [DELETE]
func (m *mux) unfriend(c *echo.Context) error {
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
		return relationshipError(c, err)
	}

	return c.NoContent(204)
}

// acceptFriend
//
// @summary accept friend request
// @description accepts pending friend request from another user
// @tags me
// @accept json
// @failure 413 {object} jsonerr.JSONError "request body exceeds 16 KiB"
// @failure 415 {object} jsonerr.JSONError "content type must be application/json"
// @param user body friendRequest true "user to accept"
// @success 200 {object} friendDetails
// @failure 400 {object} jsonerr.JSONError "invalid request"
// @failure 401 {object} jsonerr.JSONError "invalid token"
// @failure 404 {object} jsonerr.JSONError "requested user not exists"
// @failure 409 {object} jsonerr.JSONError "friend or pending request limit reached"
// @failure 500 {object} jsonerr.JSONError "internal server error"
// @failure 503 {object} jsonerr.JSONError "server busy; retry after Retry-After seconds"
// @header 503 {string} Retry-After "Minimum delay in seconds before retrying (1)"
// @router /me/friend/accept [POST]
func (m *mux) acceptFriend(c *echo.Context) error {
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
		return relationshipError(c, err)
	}

	currentUser, err := m.userAdapter.GetUser(ctx, request.UserID())
	if err != nil {
		return jsonerr.EchoInternalError(err).Echo(c)
	}

	friendSinceTime := currentUser.FriendSinceFor(requester.ID)
	return c.JSON(http.StatusOK, newFriendDetails(
		requester.Auth.Username, requester.Status, friendStateAccepted, friendSinceTime,
	))
}

// rejectFriend
//
// @summary reject friend request
// @description rejects pending friend request from another user
// @tags me
// @accept json
// @failure 413 {object} jsonerr.JSONError "request body exceeds 16 KiB"
// @failure 415 {object} jsonerr.JSONError "content type must be application/json"
// @param user body friendRequest true "user to reject"
// @success 204
// @failure 400 {object} jsonerr.JSONError "invalid request"
// @failure 401 {object} jsonerr.JSONError "invalid token"
// @failure 404 {object} jsonerr.JSONError "requested user not exists"
// @failure 500 {object} jsonerr.JSONError "internal server error"
// @failure 503 {object} jsonerr.JSONError "server busy; retry after Retry-After seconds"
// @header 503 {string} Retry-After "Minimum delay in seconds before retrying (1)"
// @router /me/friend/reject [POST]
func (m *mux) rejectFriend(c *echo.Context) error {
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
		if errors.Is(err, users.ErrFriendRequestNotExists) {
			return jsonerr.EchoNotFoundError(err).Echo(c)
		}
		return relationshipError(c, err)
	}

	return c.NoContent(http.StatusNoContent)
}

// stopSharing
//
// @summary stop sharing location
// @description stop sharing location with another user
// @tags me
// @accept json
// @failure 413 {object} jsonerr.JSONError "request body exceeds 16 KiB"
// @failure 415 {object} jsonerr.JSONError "content type must be application/json"
// @param user body friendRequest true "user to stop sharing with"
// @success 204
// @failure 400 {object} jsonerr.JSONError "invalid request"
// @failure 500 {object} jsonerr.JSONError "internal server error"
// @failure 503 {object} jsonerr.JSONError "server busy; retry after Retry-After seconds"
// @header 503 {string} Retry-After "Minimum delay in seconds before retrying (1)"
// @router /me/sharing/stop [POST]
func (m *mux) stopSharing(c *echo.Context) error {
	request, bindErr := binder.BindRequest[friendRequest](c, true)
	if bindErr != nil {
		return bindErr.Echo(c)
	}
	defer request.Cancel()
	ctx := request.Context()

	target, err := m.userAdapter.GetUserByUsername(
		ctx,
		request.Request.Username,
	)
	if err != nil {
		if errors.Is(err, users.ErrUserNotExists) {
			return jsonerr.EchoError(http.StatusBadRequest, "user to stop sharing with not found", nil).Echo(c)
		}
		return jsonerr.EchoInternalError(err).Echo(c)
	}

	err = m.userAdapter.StopSharing(
		ctx,
		request.UserID(),
		target.ID,
	)
	if err != nil {
		return jsonerr.EchoInternalError(err).Echo(c)
	}

	return c.NoContent(http.StatusNoContent)
}

// resumeSharing
//
// @summary resume sharing location
// @description resume sharing location with another user
// @tags me
// @accept json
// @failure 413 {object} jsonerr.JSONError "request body exceeds 16 KiB"
// @failure 415 {object} jsonerr.JSONError "content type must be application/json"
// @param user body friendRequest true "user to resume sharing with"
// @success 204
// @failure 400 {object} jsonerr.JSONError "invalid request"
// @failure 500 {object} jsonerr.JSONError "internal server error"
// @failure 503 {object} jsonerr.JSONError "server busy; retry after Retry-After seconds"
// @header 503 {string} Retry-After "Minimum delay in seconds before retrying (1)"
// @router /me/sharing/resume [POST]
func (m *mux) resumeSharing(c *echo.Context) error {
	request, bindErr := binder.BindRequest[friendRequest](c, true)
	if bindErr != nil {
		return bindErr.Echo(c)
	}
	defer request.Cancel()
	ctx := request.Context()

	target, err := m.userAdapter.GetUserByUsername(
		ctx,
		request.Request.Username,
	)
	if err != nil {
		if errors.Is(err, users.ErrUserNotExists) {
			return jsonerr.EchoError(http.StatusBadRequest, "user to start sharing with not found", nil).Echo(c)
		}
		return jsonerr.EchoInternalError(err).Echo(c)
	}

	err = m.userAdapter.ResumeSharing(
		ctx,
		request.UserID(),
		target.ID,
	)
	if err != nil {
		return jsonerr.EchoInternalError(err).Echo(c)
	}

	return c.NoContent(http.StatusNoContent)
}

// getPaused
//
// @summary get paused friends details
// @description returns list of friends with whom location sharing is paused
// @tags me
// @produce json
// @success 200 {object} getPausedResponse
// @failure 401 {object} jsonerr.JSONError "invalid token"
// @failure 500 {object} jsonerr.JSONError "internal server error"
// @failure 503 {object} jsonerr.JSONError "server busy; retry after Retry-After seconds"
// @header 503 {string} Retry-After "Minimum delay in seconds before retrying (1)"
// @router /me/sharing [GET]
func (m *mux) getPaused(c *echo.Context) error {
	request, bindErr := binder.BindRequest[binder.EmptyBody](c, true)
	if bindErr != nil {
		return bindErr.Echo(c)
	}
	defer request.Cancel()

	ctx := request.Context()

	result := make(getPausedResponse, 0)

	pausedUsers, err := m.userAdapter.GetPausedUsers(ctx, request.UserID())
	if err != nil {
		return jsonerr.EchoInternalError(err).Echo(c)
	}

	for _, f := range pausedUsers {
		pausedFriend := pausedFriendDetails{
			Username: f.Username,
		}
		result = append(result, pausedFriend)
	}

	return c.JSON(http.StatusOK, result)
}
