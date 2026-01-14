package utils

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
)

const (
	// MaxFileSize representa o tamanho máximo permitido após decodificação (7.5MB)
	MaxFileSize = 7.5 * 1024 * 1024

	// MaxBase64Size representa o tamanho máximo da string base64 (10MB)
	MaxBase64Size = 10 * 1024 * 1024
)

// Base64FileInfo contém informações do arquivo decodificado
type Base64FileInfo struct {
	DecodedData []byte
	MimeType    string
	Size        int64
	TempPath    string
}

// ValidateBase64 valida se a string é um base64 válido
func ValidateBase64(data string) error {
	if len(data) == 0 {
		return errors.New("conteúdo base64 não pode estar vazio")
	}

	if len(data) > MaxBase64Size {
		return fmt.Errorf("tamanho da string base64 excede o limite de %d bytes", MaxBase64Size)
	}

	// Tenta decodificar para validar formato
	_, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return fmt.Errorf("formato base64 inválido: %v", err)
	}

	return nil
}

// DecodeBase64File decodifica base64 e retorna informações do arquivo
func DecodeBase64File(base64Data string) (*Base64FileInfo, error) {
	// Validar base64
	if err := ValidateBase64(base64Data); err != nil {
		return nil, err
	}

	// Decodificar
	decodedData, err := base64.StdEncoding.DecodeString(base64Data)
	if err != nil {
		return nil, fmt.Errorf("erro ao decodificar base64: %v", err)
	}

	// Verificar tamanho após decodificação
	if len(decodedData) > MaxFileSize {
		return nil, fmt.Errorf("tamanho do arquivo após decodificação excede o limite de %.1f MB", MaxFileSize/(1024*1024))
	}

	// Detectar MIME type usando amostra adequada
	sampleSize := len(decodedData)
	if sampleSize > 512 {
		sampleSize = 512
	}
	mimeType := http.DetectContentType(decodedData[:sampleSize])

	// Criar arquivo temporário
	tempFile, err := os.CreateTemp("", "docsigner_base64_*")
	if err != nil {
		return nil, fmt.Errorf("erro ao criar arquivo temporário: %v", err)
	}

	// Escrever dados no arquivo temporário
	if _, err := tempFile.Write(decodedData); err != nil {
		tempFile.Close()
		os.Remove(tempFile.Name())
		return nil, fmt.Errorf("erro ao escrever arquivo temporário: %v", err)
	}

	if err := tempFile.Close(); err != nil {
		os.Remove(tempFile.Name())
		return nil, fmt.Errorf("erro ao fechar arquivo temporário: %v", err)
	}

	return &Base64FileInfo{
		DecodedData: decodedData,
		MimeType:    mimeType,
		Size:        int64(len(decodedData)),
		TempPath:    tempFile.Name(),
	}, nil
}

// CleanupTempFile remove o arquivo temporário criado
func CleanupTempFile(tempPath string) error {
	if tempPath == "" {
		return nil
	}

	if err := os.Remove(tempPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("erro ao remover arquivo temporário %s: %v", tempPath, err)
	}

	return nil
}

// ValidateMimeType verifica se o MIME type é suportado
func ValidateMimeType(mimeType string) error {
	supportedTypes := map[string]bool{
		"application/pdf": true,
		"image/jpeg":      true,
		"image/jpg":       true,
		"image/png":       true,
		"image/gif":       true,
	}

	if !supportedTypes[mimeType] {
		return fmt.Errorf("tipo de arquivo não suportado: %s", mimeType)
	}

	return nil
}

// GetFileExtensionFromMimeType retorna a extensão baseada no MIME type
func GetFileExtensionFromMimeType(mimeType string) string {
	extensions := map[string]string{
		"application/pdf": ".pdf",
		"image/jpeg":      ".jpg",
		"image/jpg":       ".jpg",
		"image/png":       ".png",
		"image/gif":       ".gif",
	}

	if ext, exists := extensions[mimeType]; exists {
		return ext
	}

	return ".bin"
}
