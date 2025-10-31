package clicksign

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"app/infrastructure/clicksign/dto"

	"github.com/sirupsen/logrus"
)

type RequirementService struct {
	clicksignClient ClicksignClientInterface
	logger          *logrus.Logger
}

func NewRequirementService(clicksignClient ClicksignClientInterface, logger *logrus.Logger) *RequirementService {
	return &RequirementService{
		clicksignClient: clicksignClient,
		logger:          logger,
	}
}

// CreateRequirement cria um requisito no envelope usando a estrutura JSON API com relacionamentos
func (s *RequirementService) CreateRequirement(ctx context.Context, envelopeID string, reqData RequirementData) (string, error) {

	createRequest := s.mapRequirementDataToCreateRequest(reqData)

	// Fazer chamada para API do Clicksign usando o endpoint correto para requisitos
	endpoint := fmt.Sprintf("/api/v3/envelopes/%s/requirements", envelopeID)
    resp, err := s.clicksignClient.Post(ctx, endpoint, createRequest)

	if err != nil {
        if ce, ok := err.(*ClicksignError); ok {
            return "", ce
        }
        // Log the error with more context
        return "", fmt.Errorf("failed to create requirement in Clicksign envelope: %w", err)
	}
	defer resp.Body.Close()

	// Ler resposta
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response from Clicksign: %w body: %s", err, body)
	}

	// Verificar se houve erro na resposta
    if resp.StatusCode >= 400 {
        var errorResp dto.ClicksignErrorResponse
        // tentar decodificar, mas ignorar falhas (queremos preservar body cru na mensagem)
        _ = json.Unmarshal(body, &errorResp)
        return "", &ClicksignError{
            Type:       s.categorizeHTTPError(resp.StatusCode),
            Message:    fmt.Sprintf("Clicksign API error (status %d): %s", resp.StatusCode, string(body)),
            StatusCode: resp.StatusCode,
        }
    }

	// Fazer parse da resposta de sucesso usando estrutura JSON API
	var createResponse dto.RequirementCreateResponseWrapper
    if err := json.Unmarshal(body, &createResponse); err != nil {
        return "", &ClicksignError{Type: ErrorTypeSerialization, Message: "failed to parse JSON API response from Clicksign", Original: err}
    }

	return createResponse.Data.ID, nil
}

// CreateBulkRequirements cria múltiplos requisitos usando operações atômicas conforme JSON API spec
func (s *RequirementService) CreateBulkRequirements(ctx context.Context, envelopeID string, operations []BulkOperation) ([]string, error) {

	bulkRequest := s.mapBulkOperationsToBulkRequest(operations)

	// Fazer chamada para API do Clicksign usando o endpoint de bulk requirements
	endpoint := fmt.Sprintf("/api/v3/envelopes/%s/bulk_requirements", envelopeID)
    resp, err := s.clicksignClient.Post(ctx, endpoint, bulkRequest)
	if err != nil {
        if ce, ok := err.(*ClicksignError); ok {
            return nil, ce
        }
        return nil, fmt.Errorf("failed to create bulk requirements in Clicksign envelope: %w", err)
	}
	defer resp.Body.Close()

	// Ler resposta
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response from Clicksign: %w", err)
	}

	// Verificar se houve erro na resposta
    if resp.StatusCode >= 400 {
        return nil, &ClicksignError{
            Type:       s.categorizeHTTPError(resp.StatusCode),
            Message:    fmt.Sprintf("Clicksign API error (status %d): %s", resp.StatusCode, string(body)),
            StatusCode: resp.StatusCode,
        }
    }

	// Fazer parse da resposta de sucesso usando estrutura JSON API para bulk operations
	var bulkResponse dto.BulkRequirementsResponseWrapper
    if err := json.Unmarshal(body, &bulkResponse); err != nil {
        return nil, &ClicksignError{Type: ErrorTypeSerialization, Message: "failed to parse JSON API bulk response from Clicksign", Original: err}
    }

	// Extrair IDs dos requisitos criados
	var createdIDs []string
	for _, result := range bulkResponse.AtomicResults {
		if result.Data != nil {
			createdIDs = append(createdIDs, result.Data.ID)
		}
	}

	return createdIDs, nil
}

// mapRequirementDataToCreateRequest mapeia os dados do requisito para a estrutura JSON API
func (s *RequirementService) mapRequirementDataToCreateRequest(reqData RequirementData) *dto.RequirementCreateRequestWrapper {
	req := &dto.RequirementCreateRequestWrapper{
		Data: dto.RequirementCreateData{
			Type: "requirements",
			Attributes: dto.RequirementCreateAttributes{
				Action: reqData.Action,
			},
		},
	}

	// Adicionar role se fornecido (usado para qualificação)
	if reqData.Role != "" {
		req.Data.Attributes.Role = reqData.Role
	}

	// Adicionar auth se fornecido (usado para autenticação)
	if reqData.Auth != "" {
		req.Data.Attributes.Auth = reqData.Auth
	}

	// Adicionar relacionamentos se fornecidos
	if reqData.DocumentID != "" || reqData.SignerID != "" {
		req.Data.Relationships = &dto.RequirementRelationships{}

		if reqData.DocumentID != "" {
			req.Data.Relationships.Document = &dto.RequirementRelationship{
				Data: dto.RequirementRelationshipData{
					Type: "documents",
					ID:   reqData.DocumentID,
				},
			}
		}

		if reqData.SignerID != "" {
			req.Data.Relationships.Signer = &dto.RequirementRelationship{
				Data: dto.RequirementRelationshipData{
					Type: "signers",
					ID:   reqData.SignerID,
				},
			}
		}
	}

	return req
}

// mapBulkOperationsToBulkRequest mapeia operações em lote para a estrutura de atomic operations
func (s *RequirementService) mapBulkOperationsToBulkRequest(operations []BulkOperation) *dto.BulkRequirementsRequestWrapper {
	atomicOps := make([]dto.AtomicOperation, len(operations))

	for i, op := range operations {
		atomicOp := dto.AtomicOperation{
			Op: op.Operation,
		}

		if op.Operation == "remove" && op.RequirementID != "" {
			atomicOp.Ref = &dto.AtomicOperationRef{
				Type: "requirements",
				ID:   op.RequirementID,
			}
		} else if op.Operation == "add" && op.RequirementData != nil {
			attributes := dto.RequirementCreateAttributes{
				Action: op.RequirementData.Action,
				Auth:   op.RequirementData.Auth,
				Role:   op.RequirementData.Role,
			}

			atomicOp.Data = &dto.RequirementCreateData{
				Type:       "requirements",
				Attributes: attributes,
			}

			// Adicionar relacionamentos para operação add
			if op.RequirementData.DocumentID != "" || op.RequirementData.SignerID != "" {
				atomicOp.Data.Relationships = &dto.RequirementRelationships{}

				if op.RequirementData.DocumentID != "" {
					atomicOp.Data.Relationships.Document = &dto.RequirementRelationship{
						Data: dto.RequirementRelationshipData{
							Type: "documents",
							ID:   op.RequirementData.DocumentID,
						},
					}
				}

				if op.RequirementData.SignerID != "" {
					atomicOp.Data.Relationships.Signer = &dto.RequirementRelationship{
						Data: dto.RequirementRelationshipData{
							Type: "signers",
							ID:   op.RequirementData.SignerID,
						},
					}
				}
			}
		}

		atomicOps[i] = atomicOp
	}

	return &dto.BulkRequirementsRequestWrapper{
		AtomicOperations: atomicOps,
	}
}

// categorizeHTTPError categoriza erros baseados no status code HTTP
func (s *RequirementService) categorizeHTTPError(statusCode int) string {
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

// RequirementData representa os dados necessários para criar um requisito
type RequirementData struct {
	Action     string // "agree" para qualificação ou "provide_evidence" para autenticação
	Role       string // "sign" para qualificação
	Auth       string // "email" ou "icp_brasil" para autenticação
	DocumentID string // ID do documento relacionado
	SignerID   string // ID do signatário relacionado
}

// BulkOperation representa uma operação em lote para requisitos
type BulkOperation struct {
	Operation       string           // "add" ou "remove"
	RequirementID   string           // ID do requisito para operação "remove"
	RequirementData *RequirementData // Dados do requisito para operação "add"
}
