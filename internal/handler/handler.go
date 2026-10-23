// Package handler предоставляет семь пользовательских HTTP-методов Gophermart.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/mikkims/ya_diploma_1/internal/auth"
	"github.com/mikkims/ya_diploma_1/internal/gzip"
	"github.com/mikkims/ya_diploma_1/internal/logger"
	"github.com/mikkims/ya_diploma_1/internal/model"
	"github.com/mikkims/ya_diploma_1/internal/model/dto"
	"github.com/rs/zerolog"
)

type Service interface {
	Register(context.Context, string, string) (int64, error)
	Login(context.Context, string, string) (int64, error)
	AddOrder(context.Context, int64, string) (bool, error)
	Orders(context.Context, int64) ([]model.Order, error)
	Balance(context.Context, int64) (model.Balance, error)
	Withdraw(context.Context, int64, string, model.Money) error
	Withdrawals(context.Context, int64) ([]model.Withdrawal, error)
}

type handler struct {
	service Service
	auth    *auth.Auth
	log     zerolog.Logger
}

// NewRouter связывает API, аутентификацию, gzip и безопасное логирование.
func NewRouter(s Service, a *auth.Auth, log zerolog.Logger) http.Handler {
	h := &handler{service: s, auth: a, log: log}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Header("X-Content-Type-Options", "nosniff")
		c.Next()
	})
	r.Use(gin.CustomRecoveryWithWriter(io.Discard, func(c *gin.Context, _ any) { c.AbortWithStatus(http.StatusInternalServerError) }))
	r.POST("/api/user/register", h.register)
	r.POST("/api/user/login", h.login)
	private := r.Group("/api/user", h.authorize)
	private.POST("/orders", h.addOrder)
	private.GET("/orders", h.orders)
	private.GET("/balance", h.balance)
	private.POST("/balance/withdraw", h.withdraw)
	private.GET("/withdrawals", h.withdrawals)
	return logger.Middleware(log)(gzip.MiddlewareGzip(log)(r))
}

func (h *handler) authorize(c *gin.Context) {
	value := ""
	if header := c.GetHeader("Authorization"); header != "" {
		parts := strings.Fields(header)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		value = parts[1]
	} else {
		cookie, err := c.Request.Cookie("gophermart_token")
		if err != nil {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		value = cookie.Value
	}
	id, ok := h.auth.UserID(value)
	if !ok {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	c.Set("userID", id)
	c.Next()
}

func decode(c *gin.Context, dest any) bool {
	media, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || media != "application/json" {
		c.Status(http.StatusBadRequest)
		return false
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16384)
	d := json.NewDecoder(c.Request.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(dest); err != nil {
		c.Status(http.StatusBadRequest)
		return false
	}
	if d.Decode(new(any)) != io.EOF {
		c.Status(http.StatusBadRequest)
		return false
	}
	return true
}

func (h *handler) respondError(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, model.ErrInvalidInput):
		status = http.StatusBadRequest
	case errors.Is(err, model.ErrUnauthorized):
		status = http.StatusUnauthorized
	case errors.Is(err, model.ErrLoginTaken), errors.Is(err, model.ErrOrderOwned):
		status = http.StatusConflict
	case errors.Is(err, model.ErrInvalidOrder):
		status = http.StatusUnprocessableEntity
	case errors.Is(err, model.ErrInsufficient):
		status = http.StatusPaymentRequired
	}
	if status == http.StatusInternalServerError {
		logger.Failure(h.log.Error(), c.FullPath(), err).Msg("API operation failed")
	}
	c.Status(status)
}

func (h *handler) authenticated(c *gin.Context, id int64) {
	token, err := h.auth.Token(id)
	if err != nil {
		h.respondError(c, err)
		return
	}
	http.SetCookie(c.Writer, &http.Cookie{
		Name:  "gophermart_token",
		Value: token, Path: "/",
		HttpOnly: true,
		Secure:   c.Request.TLS != nil,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   86400,
		Expires:  time.Now().Add(24 * time.Hour)})
	c.Header("Authorization", "Bearer "+token)
	c.Status(http.StatusOK)
}

func (h *handler) register(c *gin.Context) {
	var req dto.Credentials
	if !decode(c, &req) {
		return
	}
	id, err := h.service.Register(c.Request.Context(), req.Login, req.Password)
	if err != nil {
		h.respondError(c, err)
		return
	}
	h.authenticated(c, id)
}

func (h *handler) login(c *gin.Context) {
	var req dto.Credentials
	if !decode(c, &req) {
		return
	}
	id, err := h.service.Login(c.Request.Context(), req.Login, req.Password)
	if err != nil {
		h.respondError(c, err)
		return
	}
	h.authenticated(c, id)
}

func (h *handler) addOrder(c *gin.Context) {
	media, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || media != "text/plain" {
		c.Status(http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 16384))
	if err != nil || len(body) == 0 {
		c.Status(http.StatusBadRequest)
		return
	}
	created, err := h.service.AddOrder(c.Request.Context(), c.GetInt64("userID"), string(body))
	if err != nil {
		h.respondError(c, err)
		return
	}
	if created {
		c.Status(http.StatusAccepted)
	} else {
		c.Status(http.StatusOK)
	}
}

func (h *handler) orders(c *gin.Context) {
	items, err := h.service.Orders(c.Request.Context(), c.GetInt64("userID"))
	if err != nil {
		h.respondError(c, err)
		return
	}
	if len(items) == 0 {
		c.Status(http.StatusNoContent)
		return
	}
	c.JSON(http.StatusOK, items)
}

func (h *handler) balance(c *gin.Context) {
	value, err := h.service.Balance(c.Request.Context(), c.GetInt64("userID"))
	if err != nil {
		h.respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, value)
}

func (h *handler) withdraw(c *gin.Context) {
	var req dto.Withdraw
	if !decode(c, &req) {
		return
	}
	if err := h.service.Withdraw(c.Request.Context(), c.GetInt64("userID"), req.Order, req.Sum); err != nil {
		h.respondError(c, err)
		return
	}
	c.Status(http.StatusOK)
}

func (h *handler) withdrawals(c *gin.Context) {
	items, err := h.service.Withdrawals(c.Request.Context(), c.GetInt64("userID"))
	if err != nil {
		h.respondError(c, err)
		return
	}
	if len(items) == 0 {
		c.Status(http.StatusNoContent)
		return
	}
	c.JSON(http.StatusOK, items)
}
