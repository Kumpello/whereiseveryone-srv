package me

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"
	"whereiseveryone/internal/users"
	"whereiseveryone/internal/webapi/binder"
	"whereiseveryone/internal/webapi/jsonerr"
	"whereiseveryone/pkg/id"
)

// getPointsOfInterest
//
// @summary Get your saved points of interest
// @description At most five own points. Friends' eligible points are included in GET /me/friends.
// @tags points
// @produce json
// @success 200 {array} pointOfInterestDetail
// @failure 401 {object} jsonerr.JSONError
// @failure 500 {object} jsonerr.JSONError
// @failure 503 {object} jsonerr.JSONError
// @router /me/points [GET]
func (m *mux) getPointsOfInterest(c *echo.Context) error {
	request, bindErr := binder.BindRequest[binder.EmptyBody](c, true)
	if bindErr != nil {
		return writePointError(c, bindErr)
	}
	defer request.Cancel()
	points, err := m.userAdapter.GetPointsOfInterest(request.Context(), request.UserID())
	if err != nil {
		return pointOfInterestError(c, err)
	}
	return c.JSON(http.StatusOK, pointOfInterestDetails(points, request.TokenData().UserName))
}

// createPointOfInterest
//
// @summary Create a point of interest
// @description Up to five per user. The server assigns identity, creator and creation time.
// @tags points
// @accept json
// @produce json
// @param point body pointOfInterestRequest true "Name, optional description, and coordinates"
// @success 201 {object} pointOfInterestDetail
// @failure 400 {object} jsonerr.JSONError
// @failure 401 {object} jsonerr.JSONError
// @failure 409 {object} jsonerr.JSONError "five-point limit reached"
// @failure 413 {object} jsonerr.JSONError
// @failure 415 {object} jsonerr.JSONError
// @failure 500 {object} jsonerr.JSONError
// @failure 503 {object} jsonerr.JSONError
// @router /me/points [POST]
func (m *mux) createPointOfInterest(c *echo.Context) error {
	request, bindErr := binder.BindRequest[pointOfInterestRequest](c, true)
	if bindErr != nil {
		return writePointError(c, bindErr)
	}
	defer request.Cancel()
	if strings.TrimSpace(request.Request.Name) == "" {
		return writePointError(c, jsonerr.EchoInvalidRequestError(errors.New("point name must not be blank")))
	}
	point, err := m.userAdapter.CreatePointOfInterest(request.Context(), request.UserID(), request.Request.point())
	if err != nil {
		return pointOfInterestError(c, err)
	}
	return c.JSON(http.StatusCreated, pointOfInterestDetailFor(point, request.TokenData().UserName))
}

// updatePointOfInterest
//
// @summary Edit one of your points of interest
// @description Only the owner may edit a point. Identity and creation time remain unchanged.
// @tags points
// @accept json
// @produce json
// @param id path string true "Point ID"
// @param point body pointOfInterestRequest true "Updated name, description and coordinates"
// @success 200 {object} pointOfInterestDetail
// @failure 400 {object} jsonerr.JSONError
// @failure 401 {object} jsonerr.JSONError
// @failure 404 {object} jsonerr.JSONError
// @failure 413 {object} jsonerr.JSONError
// @failure 415 {object} jsonerr.JSONError
// @failure 500 {object} jsonerr.JSONError
// @failure 503 {object} jsonerr.JSONError
// @router /me/points/{id} [PUT]
func (m *mux) updatePointOfInterest(c *echo.Context) error {
	request, bindErr := binder.BindRequest[pointOfInterestRequest](c, true)
	if bindErr != nil {
		return writePointError(c, bindErr)
	}
	defer request.Cancel()
	pointID, err := id.FromString(c.Param("id"))
	if err != nil || pointID.IsZero() || strings.TrimSpace(request.Request.Name) == "" {
		return writePointError(c, jsonerr.EchoInvalidRequestError(errors.New("invalid point ID or name")))
	}
	point, err := m.userAdapter.UpdatePointOfInterest(
		request.Context(), request.UserID(), pointID, request.Request.point(),
	)
	if err != nil {
		return pointOfInterestError(c, err)
	}
	return c.JSON(http.StatusOK, pointOfInterestDetailFor(point, request.TokenData().UserName))
}

// deletePointOfInterest
//
// @summary Delete one of your points of interest
// @description Only the owner may delete a point; deletion frees a point slot.
// @tags points
// @param id path string true "Point ID"
// @success 204
// @failure 400 {object} jsonerr.JSONError
// @failure 401 {object} jsonerr.JSONError
// @failure 404 {object} jsonerr.JSONError
// @failure 500 {object} jsonerr.JSONError
// @failure 503 {object} jsonerr.JSONError
// @router /me/points/{id} [DELETE]
func (m *mux) deletePointOfInterest(c *echo.Context) error {
	request, bindErr := binder.BindRequest[binder.EmptyBody](c, true)
	if bindErr != nil {
		return writePointError(c, bindErr)
	}
	defer request.Cancel()
	pointID, err := id.FromString(c.Param("id"))
	if err != nil || pointID.IsZero() {
		return writePointError(c, jsonerr.EchoInvalidRequestError(errors.New("invalid point ID")))
	}
	if err := m.userAdapter.DeletePointOfInterest(request.Context(), request.UserID(), pointID); err != nil {
		return pointOfInterestError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func pointOfInterestDetails(points []users.PointOfInterest, creator string) []pointOfInterestDetail {
	result := make([]pointOfInterestDetail, 0, len(points))
	for _, point := range points {
		result = append(result, pointOfInterestDetailFor(point, creator))
	}
	return result
}

func pointOfInterestDetailFor(point users.PointOfInterest, creator string) pointOfInterestDetail {
	return pointOfInterestDetail{
		ID: point.ID.Hex(), Name: point.Name, Description: point.Description,
		Latitude: point.Latitude, Longitude: point.Longitude,
		CreatorUsername: creator, CreatedAt: newTimestamp(point.CreatedAt),
	}
}

func pointOfInterestError(c *echo.Context, err error) error {
	response := jsonerr.EchoInternalError(err)
	switch {
	case errors.Is(err, users.ErrPointsOfInterestLimit):
		response = jsonerr.EchoConflictError(err)
	case errors.Is(err, users.ErrPointOfInterestNotFound), errors.Is(err, users.ErrUserNotExists):
		response = jsonerr.EchoNotFoundError(err)
	}
	return writePointError(c, response)
}

func writePointError(c *echo.Context, response *jsonerr.JSONError) error {
	if err := response.Echo(c); err != nil {
		return fmt.Errorf("write point response: %w", err)
	}
	return nil
}

func (r pointOfInterestRequest) point() users.PointOfInterest {
	return users.PointOfInterest{
		Name: strings.TrimSpace(r.Name), Description: strings.TrimSpace(r.Description),
		Latitude: *r.Latitude, Longitude: *r.Longitude,
	}
}

type pointOfInterestRequest struct {
	Name        string   `json:"name" validate:"required,max=64"`
	Description string   `json:"description" validate:"max=1000"`
	Latitude    *float64 `json:"latitude" validate:"required,gte=-90,lte=90"`
	Longitude   *float64 `json:"longitude" validate:"required,gte=-180,lte=180"`
}

type pointOfInterestDetail struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Description     string    `json:"description"`
	Latitude        float64   `json:"latitude"`
	Longitude       float64   `json:"longitude"`
	CreatorUsername string    `json:"creator_username"`
	CreatedAt       timestamp `json:"created_at" swaggertype:"integer" format:"int64"`
}
