// Package dto определяет входные JSON-сообщения пользовательского API.
package dto

import "github.com/mikkims/ya_diploma_1/internal/model"

// Credentials содержит логин и пароль для регистрации или входа.
type Credentials struct {
	Login    string `json:"login"`
	Password string `json:"password"`
}

// Withdraw содержит заказ и положительную сумму списания.
type Withdraw struct {
	Order string      `json:"order"`
	Sum   model.Money `json:"sum"`
}
