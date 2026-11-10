package kernel

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidQuantity = errors.New("kernel: invalid quantity")
	ErrUnitMismatch    = errors.New("kernel: unit mismatch")
)

// UnitCode identifies a unit of measure that POL publishes, such as PCS or DOZEN. The kernel
// checks only its form: 1–32 characters of A–Z, 0–9 and '-'.
type UnitCode string

// Valid reports whether u has the form of a unit code.
func (u UnitCode) Valid() bool {
	if len(u) == 0 || len(u) > 32 {
		return false
	}
	for i := 0; i < len(u); i++ {
		c := u[i]
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// Quantity is a non-negative exact amount of one unit.
type Quantity struct {
	value Decimal
	unit  UnitCode
}

// NewQuantity returns value of unit.
func NewQuantity(value Decimal, unit UnitCode) (Quantity, error) {
	if !unit.Valid() {
		return Quantity{}, fmt.Errorf("%w: unit %q", ErrInvalidQuantity, unit)
	}
	if value.Sign() < 0 {
		return Quantity{}, fmt.Errorf("%w: negative value %s", ErrInvalidQuantity, value)
	}
	return Quantity{value: value, unit: unit}, nil
}

// ParseQuantity parses value in QuantityFormat (at most 6 fractional digits).
func ParseQuantity(value string, unit UnitCode) (Quantity, error) {
	d, err := ParseDecimal(value, QuantityFormat)
	if err != nil {
		return Quantity{}, fmt.Errorf("%w: %w", ErrInvalidQuantity, err)
	}
	return NewQuantity(d, unit)
}

// Value returns the amount.
func (q Quantity) Value() Decimal { return q.value }

// Unit returns the unit.
func (q Quantity) Unit() UnitCode { return q.unit }

func (q Quantity) String() string { return q.value.String() + " " + string(q.unit) }

// Cmp compares two quantities of the same unit.
func (q Quantity) Cmp(o Quantity) (int, error) {
	if q.unit != o.unit {
		return 0, ErrUnitMismatch
	}
	return q.value.Cmp(o.value), nil
}

// Add returns q + o.
func (q Quantity) Add(o Quantity) (Quantity, error) {
	if q.unit != o.unit {
		return Quantity{}, ErrUnitMismatch
	}
	return Quantity{value: q.value.Add(o.value), unit: q.unit}, nil
}

// Sub returns q − o; the result must not be negative.
func (q Quantity) Sub(o Quantity) (Quantity, error) {
	if q.unit != o.unit {
		return Quantity{}, ErrUnitMismatch
	}
	return NewQuantity(q.value.Sub(o.value), q.unit)
}

// Convert returns q in unit to, given how many units of to make one unit of q (12 for DOZEN to
// PCS). The factor comes from a published, SKU-specific conversion revision; the kernel never
// guesses one.
func (q Quantity) Convert(to UnitCode, factor Decimal) (Quantity, error) {
	if factor.Sign() <= 0 {
		return Quantity{}, fmt.Errorf("%w: conversion factor %s", ErrInvalidQuantity, factor)
	}
	return NewQuantity(q.value.Mul(factor), to)
}
