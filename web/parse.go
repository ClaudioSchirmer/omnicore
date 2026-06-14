package web

import (
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/gofiber/fiber/v2"
)

func ParseBody(c *fiber.Ctx) (domain.Fields, error) {
	var fields domain.Fields
	if err := c.BodyParser(&fields); err != nil {
		return nil, err
	}
	return fields, nil
}

func ParseID(c *fiber.Ctx) string {
	return c.Params("id")
}
