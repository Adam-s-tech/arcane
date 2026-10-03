package api

import (
	jsonv1 "encoding/json"
	"encoding/json/jsontext"
	"encoding/json/v2"

	"github.com/labstack/echo/v5"
)

type jsonV2Serializer struct{}

// Preserve the established numeric wire format for duration fields in third-party API types.
var jsonV2APIOptions = jsonv1.FormatDurationAsNano(true)

func (jsonV2Serializer) Serialize(c *echo.Context, value any, indent string) error {
	if indent != "" {
		return json.MarshalWrite(c.Response(), value, jsonV2APIOptions, jsontext.WithIndent(indent))
	}

	return json.MarshalWrite(c.Response(), value, jsonV2APIOptions)
}

func (jsonV2Serializer) Deserialize(c *echo.Context, value any) error {
	if err := json.UnmarshalRead(c.Request().Body, value, jsonV2APIOptions); err != nil {
		return echo.ErrBadRequest.Wrap(err)
	}

	return nil
}
