package clicksign

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"app/entity"
	"app/infrastructure/clicksign/dto"

	"github.com/sirupsen/logrus"
)

type EnvelopeService struct {
	clicksignClient ClicksignClientInterface
	logger          *logrus.Logger
}

func NewEnvelopeService(clicksignClient ClicksignClientInterface, logger *logrus.Logger) *EnvelopeService {
	return &EnvelopeService{
		clicksignClient: clicksignClient,
		logger:          logger,
	}
}

func (s *EnvelopeService) CreateEnvelope(ctx context.Context, envelope *entity.EntityEnvelope) (string, string, error) {

	// Mapear entidade para DTO do Clicksign
	createRequest := s.mapEntityToCreateRequest(envelope)

	// Fazer chamada para API do Clicksign
	resp, err := s.clicksignClient.Post(ctx, "/api/v3/envelopes", createRequest)
	if err != nil {
		// Se já veio um ClicksignError do client, apenas propaga
		if ce, ok := err.(*ClicksignError); ok {
			return "", "", ce
		}
		return "", "", fmt.Errorf("failed to create envelope in Clicksign: %w", err)
	}
	defer resp.Body.Close()

	// Ler resposta
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("failed to read response from Clicksign: %w", err)
	}

	// Verificar se houve erro na resposta
	if resp.StatusCode >= 400 {
		var errorResp dto.ClicksignErrorResponse
		_ = json.Unmarshal(body, &errorResp)
		// Retorna ClicksignError preservando status code
		return "", "", &ClicksignError{
			Type:       s.categorizeHTTPError(resp.StatusCode),
			Message:    fmt.Sprintf("Clicksign API error (status %d): %s", resp.StatusCode, string(body)),
			StatusCode: resp.StatusCode,
		}
	}

	// Preservar dados brutos antes do parse
	rawData := string(body)

	// Fazer parse da resposta de sucesso usando estrutura JSON API
	var createResponse dto.EnvelopeCreateResponseWrapper
	if err := json.Unmarshal(body, &createResponse); err != nil {
		return "", "", &ClicksignError{Type: ErrorTypeSerialization, Message: "failed to parse JSON API response from Clicksign", Original: err}
	}

	return createResponse.Data.ID, rawData, nil
}

// categorizeHTTPError categoriza erros baseados no status code HTTP
func (s *EnvelopeService) categorizeHTTPError(statusCode int) string {
	switch {
	case statusCode == 401 || statusCode == 403:
		return ErrorTypeAuthentication
	case statusCode >= 400 && statusCode < 500:
		return ErrorTypeClient
	case statusCode >= 500:
		return ErrorTypeServer
	default:
		return ErrorTypeClient
	}
}

func (s *EnvelopeService) GetEnvelope(ctx context.Context, clicksignKey string) (*dto.EnvelopeGetResponse, error) {

	endpoint := fmt.Sprintf("/api/v3/envelopes/%s", clicksignKey)
	resp, err := s.clicksignClient.Get(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("failed to get envelope from Clicksign: %w", err)
	}
	defer resp.Body.Close()

	// Ler resposta
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response from Clicksign: %w", err)
	}

	// Verificar se houve erro na resposta
	if resp.StatusCode >= 400 {
		var errorResp dto.ClicksignErrorResponse
		if err := json.Unmarshal(body, &errorResp); err != nil {
			return nil, fmt.Errorf("Clicksign API error (status %d): %s", resp.StatusCode, string(body))
		}

		if errorResp.Error.Type == "" && errorResp.Error.Message == "" {
			return nil, fmt.Errorf("Clicksign API error (status %d): %s", resp.StatusCode, string(body))
		}
		return nil, fmt.Errorf("Clicksign API error: %s - %s", errorResp.Error.Type, errorResp.Error.Message)
	}

	// Fazer parse da resposta de sucesso
	var getResponse dto.EnvelopeGetResponse
	if err := json.Unmarshal(body, &getResponse); err != nil {
		return nil, fmt.Errorf("failed to parse response from Clicksign: %w", err)
	}

	return &getResponse, nil
}

func (s *EnvelopeService) ActivateEnvelope(ctx context.Context, clicksignKey string) error {

	updateRequest := dto.EnvelopeUpdateRequestWrapper{
		Data: dto.EnvelopeUpdateRequestWrapperData{
			ID:   clicksignKey,
			Type: "envelopes",
			Attributes: dto.EnvelopeUpdateRequestWrapperAttributes{
				Status: "running",
			},
		},
	}

	endpoint := fmt.Sprintf("/api/v3/envelopes/%s", clicksignKey)
	resp, err := s.clicksignClient.Patch(ctx, endpoint, updateRequest)
	if err != nil {
		return fmt.Errorf("failed to activate envelope in Clicksign: %w", err)
	}
	defer resp.Body.Close()

	// Ler resposta
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response from Clicksign: %w", err)
	}

	// Verificar se houve erro na resposta
	if resp.StatusCode >= 400 {
		var errorResp dto.ClicksignErrorResponse
		if err := json.Unmarshal(body, &errorResp); err != nil {
			return fmt.Errorf("Clicksign API error (status %d): %s", resp.StatusCode, string(body))
		}

		return fmt.Errorf("Clicksign API error: %s - %s", errorResp.Error.Type, errorResp.Error.Message)
	}

	return nil
}

func (s *EnvelopeService) NotifyEnvelope(ctx context.Context, clicksignKey string, message string) error {
	// Estrutura da requisição de notificação
	notificationRequest := map[string]interface{}{
		"data": map[string]interface{}{
			"type": "notifications",
			"attributes": map[string]interface{}{
				"message": message,
			},
		},
	}

	endpoint := fmt.Sprintf("/api/v3/envelopes/%s/notifications", clicksignKey)
	resp, err := s.clicksignClient.Post(ctx, endpoint, notificationRequest)
	if err != nil {
		return fmt.Errorf("failed to send notification to Clicksign: %w", err)
	}
	defer resp.Body.Close()

	// Ler resposta
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response from Clicksign: %w", err)
	}

	// Verificar se houve erro na resposta
	if resp.StatusCode >= 400 {
		var errorResp dto.ClicksignErrorResponse
		if err := json.Unmarshal(body, &errorResp); err != nil {
			return fmt.Errorf("Clicksign API error (status %d): %s", resp.StatusCode, string(body))
		}

		return fmt.Errorf("Clicksign API error: %s - %s", errorResp.Error.Type, errorResp.Error.Message)
	}

	return nil
}

func (s *EnvelopeService) mapEntityToCreateRequest(envelope *entity.EntityEnvelope) *dto.EnvelopeCreateRequestWrapper {
	req := &dto.EnvelopeCreateRequestWrapper{
		Data: dto.EnvelopeCreateData{
			Type: "envelopes",
			Attributes: dto.EnvelopeCreateAttributes{
				Name:              envelope.Name,
				Locale:            "pt-BR",
				AutoClose:         envelope.AutoClose,
				RemindInterval:    envelope.RemindInterval,
				BlockAfterRefusal: true,
				DeadlineAt:        envelope.DeadlineAt,
			},
		},
	}

	if envelope.Message != "" {
		req.Data.Attributes.DefaultSubject = envelope.Message
	}

	return req
}

func stringPtr(s string) *string {
	return &s
}
