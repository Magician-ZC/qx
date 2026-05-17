package ai

// 文件说明：JSON Schema 校验器实现，负责 schema 编译缓存与结构化输出合法性验证。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/xeipuuv/gojsonschema"
)

// Validator 接口定义该模块需要实现的能力约束。
type Validator interface {
	Validate(schemaName string, schema []byte, payload []byte) error
}

// JSONSchemaValidator 结构体用于承载该模块的核心数据。
type JSONSchemaValidator struct {
	mu      sync.RWMutex
	schemas map[string]*gojsonschema.Schema
}

// NewJSONSchemaValidator 创建 JSON Schema 校验器，并初始化编译缓存。
func NewJSONSchemaValidator() *JSONSchemaValidator {
	return &JSONSchemaValidator{
		schemas: make(map[string]*gojsonschema.Schema),
	}
}

// Validate 校验响应 payload 是否满足给定 schema。
// 该流程会先做 JSON 合法性检查，再执行 schema 编译缓存与验证。
func (v *JSONSchemaValidator) Validate(schemaName string, schema []byte, payload []byte) error {
	if len(schema) == 0 {
		return errors.New("response schema is required")
	}

	if len(payload) == 0 {
		return errors.New("response payload is empty")
	}

	if !json.Valid(payload) {
		return errors.New("response payload is not valid json")
	}

	compiled, err := v.loadSchema(schemaName, schema)
	if err != nil {
		return err
	}

	result, err := compiled.Validate(gojsonschema.NewBytesLoader(payload))
	if err != nil {
		return fmt.Errorf("validate payload against schema %s: %w", effectiveSchemaName(schemaName), err)
	}

	if result.Valid() {
		return nil
	}

	failures := make([]string, 0, len(result.Errors()))
	for _, item := range result.Errors() {
		failures = append(failures, item.String())
	}

	return &SchemaValidationError{
		SchemaName: effectiveSchemaName(schemaName),
		Failures:   failures,
	}
}

// loadSchema 按 schema 内容哈希加载或编译 schema，并写入缓存。
func (v *JSONSchemaValidator) loadSchema(schemaName string, schema []byte) (*gojsonschema.Schema, error) {
	key := schemaCacheKey(schemaName, schema)

	v.mu.RLock()
	compiled := v.schemas[key]
	v.mu.RUnlock()
	if compiled != nil {
		return compiled, nil
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	if compiled = v.schemas[key]; compiled != nil {
		return compiled, nil
	}

	compiled, err := gojsonschema.NewSchema(gojsonschema.NewBytesLoader(schema))
	if err != nil {
		return nil, fmt.Errorf("compile schema %s: %w", effectiveSchemaName(schemaName), err)
	}

	v.schemas[key] = compiled
	return compiled, nil
}

// SchemaValidationError 结构体用于承载该模块的核心数据。
type SchemaValidationError struct {
	SchemaName string
	Failures   []string
}

// Error 返回聚合后的 schema 校验失败信息。
func (e *SchemaValidationError) Error() string {
	return fmt.Sprintf("schema %s validation failed: %s", e.SchemaName, strings.Join(e.Failures, "; "))
}

// schemaCacheKey 生成 schema 编译缓存键（名称 + 内容哈希）。
func schemaCacheKey(schemaName string, schema []byte) string {
	hash := sha256.Sum256(schema)
	return effectiveSchemaName(schemaName) + ":" + hex.EncodeToString(hash[:])
}

// effectiveSchemaName 返回可展示的 schema 名称（为空时使用默认名）。
func effectiveSchemaName(schemaName string) string {
	if schemaName == "" {
		return "qunxiang_response"
	}

	return schemaName
}
