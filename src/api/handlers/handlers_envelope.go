package handlers

import (
    "errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"app/api/handlers/dtos"
	"app/config"
	"app/entity"
	"app/infrastructure/clicksign"
	"app/infrastructure/repository"
	"app/pkg/utils"
	"app/usecase/document"
	usecase_envelope "app/usecase/envelope"
	"app/usecase/requirement"
	"app/usecase/signatory"
	"app/usecase/webhook"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

type EnvelopeHandlers struct {
	UsecaseEnvelope    usecase_envelope.IUsecaseEnvelope
	UsecaseDocuments   document.IUsecaseDocument
	UsecaseRequirement requirement.IUsecaseRequirement
	UsecaseSignatory   signatory.IUsecaseSignatory
	UsecaseWebhook     webhook.UsecaseWebhookInterface
	Logger             *logrus.Logger
}

func NewEnvelopeHandler(usecaseEnvelope usecase_envelope.IUsecaseEnvelope, usecaseDocuments document.IUsecaseDocument, usecaseRequirement requirement.IUsecaseRequirement, usecaseSignatory signatory.IUsecaseSignatory, logger *logrus.Logger) *EnvelopeHandlers {
	return &EnvelopeHandlers{
		UsecaseEnvelope:    usecaseEnvelope,
		UsecaseDocuments:   usecaseDocuments,
		UsecaseRequirement: usecaseRequirement,
		UsecaseSignatory:   usecaseSignatory,
		Logger:             logger,
	}
}

// @Summary Create envelope
// @Description Create a new envelope in Clicksign with optional signatories. When signatories are provided in the request, they will be created along with the envelope in a single atomic transaction. The process maintains backward compatibility - envelopes can still be created without signatories. The response includes the complete raw data returned by Clicksign API for debugging and analysis purposes.
// @Tags envelopes
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param request body dtos.EnvelopeCreateRequestDTO true "Envelope data with optional signatories array. When signatories are provided, the response will include the created signatories with their IDs."
// @Success 201 {object} dtos.EnvelopeResponseDTO "Envelope created successfully. The response includes clicksign_raw_data field with the complete JSON response from Clicksign API (optional field for debugging). If signatories were provided in the request, the response includes the created signatories with their assigned IDs."
// @Failure 400 {object} dtos.ValidationErrorResponseDTO "Validation error - invalid request data, duplicate signatory emails, or unsupported document format"
// @Failure 500 {object} dtos.ErrorResponseDTO "Internal server error - envelope creation failed or signatory creation failed during transaction"
// @Router /api/v1/envelopes [post]
func (h *EnvelopeHandlers) CreateEnvelopeHandler(c *gin.Context) {
	correlationID := c.GetHeader("X-Correlation-ID")
	if correlationID == "" {
		correlationID = strconv.FormatInt(time.Now().Unix(), 10)
	}

	var requestDTO dtos.EnvelopeCreateRequestDTO

	if err := c.ShouldBindJSON(&requestDTO); err != nil {
		validationErrors := h.extractValidationErrors(err)
		c.JSON(http.StatusBadRequest, dtos.ValidationErrorResponseDTO{
			Error:   "Validation failed",
			Message: "Invalid request payload",
			Details: validationErrors,
		})
		return
	}

	// Validação customizada do DTO
	if err := requestDTO.Validate(); err != nil {
		c.JSON(http.StatusBadRequest, dtos.ErrorResponseDTO{
			Error:   "Validation failed",
			Message: err.Error(),
		})
		return
	}

	// Converter DTO para entidade
	envelope, documents, err := h.mapCreateRequestToEntity(requestDTO)
	if err != nil {
		c.JSON(http.StatusBadRequest, dtos.ErrorResponseDTO{
			Error:   "Invalid request",
			Message: err.Error(),
		})
		return
	}

	// Limpar arquivos temporários em caso de erro
	var tempPaths []string
	for _, doc := range documents {
		if doc.IsFromBase64 && doc.FilePath != "" {
			tempPaths = append(tempPaths, doc.FilePath)
		}
	}
	defer func() {
		for _, tempPath := range tempPaths {
			if cleanupErr := utils.CleanupTempFile(tempPath); cleanupErr != nil {
				h.Logger.Warn("Failed to cleanup temporary file")
			}
		}
	}()

	// Criar envelope através do use case
	var createdEnvelope *entity.EntityEnvelope

	createdEnvelope, err = h.UsecaseEnvelope.CreateEnvelope(envelope)

	// if len(documents) > 0 {
	// 	// Criar envelope com documentos base64
	// 	createdEnvelope, err = h.UsecaseEnvelope.CreateEnvelopeWithDocuments(envelope, documents)
	// } else {
	// 	// Criar envelope com IDs de documentos existentes

	// }

    if err != nil {
        status := http.StatusInternalServerError
        var ce *clicksign.ClicksignError
        if errors.As(err, &ce) && ce.StatusCode > 0 {
            status = ce.StatusCode
        }
        c.JSON(status, dtos.ErrorResponseDTO{
            Error:   http.StatusText(status),
            Message: "Failed to create envelope: " + err.Error(),
            Details: map[string]interface{}{
                "correlation_id": correlationID,
            },
        })
        return
    }

	// cria o envelope com documentos base64
	if len(documents) > 0 {

		for _, doc := range documents {

			err := h.UsecaseDocuments.Create(doc)
			if err != nil {
				c.JSON(http.StatusInternalServerError, dtos.ErrorResponseDTO{
					Error:   "Internal server error",
					Message: fmt.Sprintf("Failed to create document '%s': %v", doc.Name, err),
					Details: map[string]interface{}{
						"correlation_id": correlationID,
					},
				})
				return
			}
			// Adicionar documento ao envelope criado
			createdEnvelope.DocumentsIDs = append(createdEnvelope.DocumentsIDs, doc.ID)

			// Enviar documento para o Clicksign e obter o clicksign_key
			doc.ClicksignKey, err = h.UsecaseEnvelope.CreateDocument(
				c.Request.Context(),
				createdEnvelope.ClicksignKey,
				doc,
				createdEnvelope.ID,
			)

			if err != nil {
				c.JSON(http.StatusInternalServerError, dtos.ErrorResponseDTO{
					Error:   "Internal server error",
					Message: fmt.Sprintf("Failed to upload document '%s' to Clicksign: %v", doc.Name, err),
					Details: map[string]interface{}{
						"correlation_id": correlationID,
					},
				})
				return
			}

			// Atualizar documento no banco com o clicksign_key
			err = h.UsecaseDocuments.Update(doc)
			if err != nil {
				c.JSON(http.StatusInternalServerError, dtos.ErrorResponseDTO{
					Error:   "Internal server error",
					Message: fmt.Sprintf("Failed to update document '%s' with Clicksign key: %v", doc.Name, err),
					Details: map[string]interface{}{
						"correlation_id": correlationID,
					},
				})
				return
			}
		}

		// Atualizar envelope no banco com os IDs dos documentos
		err = h.UsecaseEnvelope.UpdateEnvelope(createdEnvelope)
		if err != nil {
			c.JSON(http.StatusInternalServerError, dtos.ErrorResponseDTO{
				Error:   "Internal server error",
				Message: fmt.Sprintf("Failed to update envelope with document IDs: %v", err),
				Details: map[string]interface{}{
					"correlation_id": correlationID,
				},
			})
			return
		}
	}

	// Criar signatários se fornecidos no request
	var createdSignatories []entity.EntitySignatory
	if len(requestDTO.Signatories) > 0 {
		for i, signatoryRequest := range requestDTO.Signatories {
			// Converter EnvelopeSignatoryRequest para SignatoryCreateRequestDTO
			signatoryDTO := signatoryRequest.ToSignatoryCreateRequestDTO(createdEnvelope.ID)

			// Converter DTO para entidade
			signatoryEntity := signatoryDTO.ToEntity()

			// Criar signatário através do use case
			createdSignatory, sigErr := h.UsecaseSignatory.CreateSignatory(&signatoryEntity)
			if sigErr != nil {
				// FIXME: Rollback automático de envelope não implementado
				// Considerar implementação futura de transação distribuída
				c.JSON(http.StatusInternalServerError, dtos.ErrorResponseDTO{
					Error:   "Internal server error",
					Message: fmt.Sprintf("Failed to create signatory %d: %v. ATENÇÃO: Envelope %d foi criado mas signatários falharam", i+1, sigErr, createdEnvelope.ID),
					Details: map[string]interface{}{
						"correlation_id":      correlationID,
						"envelope_id":         createdEnvelope.ID,
						"failed_signatory":    i + 1,
						"partial_transaction": true,
					},
				})
				return
			}

			createdSignatories = append(createdSignatories, *createdSignatory)
		}
	}

	// cria os requirements se fornecidos no request
	if len(requestDTO.Requirements) > 0 {
		for i, requirementRequest := range requestDTO.Requirements {

			// Verificar se há signatários suficientes
			if i >= len(createdSignatories) {
				c.JSON(http.StatusBadRequest, dtos.ErrorResponseDTO{
					Error:   "Bad Request",
					Message: fmt.Sprintf("Não há signatários suficientes para o requirement %d. Enviados: %d, Necessários: %d", i+1, len(createdSignatories), len(requestDTO.Requirements)),
					Details: map[string]interface{}{
						"correlation_id": correlationID,
						"envelope_id":    createdEnvelope.ID,
					},
				})
				return
			}

			signatory := createdSignatories[i]

			for _, document := range documents {
				// Converter RequirementCreateRequest para EntityRequirement
				_, err := h.UsecaseRequirement.CreateRequirement(c.Request.Context(), &entity.EntityRequirement{
					EnvelopeID:   createdEnvelope.ID,
					ClicksignKey: createdEnvelope.ClicksignKey,
					DocumentID:   &document.ClicksignKey,
					SignerID:     &signatory.ClicksignKey,
					Action:       requirementRequest.Action,
					Auth:         requirementRequest.Auth,
				})

                if err != nil {
                    status := http.StatusInternalServerError
                    var ce *clicksign.ClicksignError
                    if errors.As(err, &ce) && ce.StatusCode > 0 {
                        status = ce.StatusCode
                    }
                    c.JSON(status, dtos.ErrorResponseDTO{
                        Error:   http.StatusText(status),
                        Message: fmt.Sprintf("Failed to create requirement for envelope %d: %v", createdEnvelope.ID, err),
                        Details: map[string]interface{}{
                            "correlation_id": correlationID,
                            "envelope_id":    createdEnvelope.ID,
                        },
                    })
                    return
                }
			}
		}
	}

	// cria os qualificadores se fornecidos no request
	if len(requestDTO.Qualifiers) > 0 {
		for i, qualifierRequest := range requestDTO.Qualifiers {
			// Verificar se há signatários suficientes
			if i >= len(createdSignatories) {
				c.JSON(http.StatusBadRequest, dtos.ErrorResponseDTO{
					Error:   "Bad Request",
					Message: fmt.Sprintf("Não há signatários suficientes para o qualifier %d. Enviados: %d, Necessários: %d", i+1, len(createdSignatories), len(requestDTO.Qualifiers)),
					Details: map[string]interface{}{
						"correlation_id": correlationID,
						"envelope_id":    createdEnvelope.ID,
					},
				})
				return
			}

			signatory := createdSignatories[i]

			for _, document := range documents {
				// Converter RequirementCreateRequest para EntityRequirement

				_, err := h.UsecaseRequirement.CreateRequirement(c.Request.Context(), &entity.EntityRequirement{
					EnvelopeID:   createdEnvelope.ID,
					ClicksignKey: createdEnvelope.ClicksignKey,
					DocumentID:   &document.ClicksignKey,
					SignerID:     &signatory.ClicksignKey,
					Action:       qualifierRequest.Action,
					Role:         qualifierRequest.Role,
				})

                if err != nil {
                    status := http.StatusInternalServerError
                    var ce *clicksign.ClicksignError
                    if errors.As(err, &ce) && ce.StatusCode > 0 {
                        status = ce.StatusCode
                    }
                    c.JSON(status, dtos.ErrorResponseDTO{
                        Error:   http.StatusText(status),
                        Message: fmt.Sprintf("Failed to create requirement for envelope %d: %v", createdEnvelope.ID, err),
                        Details: map[string]interface{}{
                            "correlation_id": correlationID,
                            "envelope_id":    createdEnvelope.ID,
                        },
                    })
                    return
                }
			}
		}
	}

	if requestDTO.Approved {
		// Ativar envelope se aprovado
		createdEnvelope, err = h.UsecaseEnvelope.ActivateEnvelope(createdEnvelope.ID)
		if err != nil {
			log.Println("Failed to activate envelope____________________________:", err)
			c.JSON(http.StatusInternalServerError, dtos.ErrorResponseDTO{
				Error:   "Internal server error",
				Message: fmt.Sprintf("Failed to activate envelope %v: %v", createdEnvelope.ClicksignKey, err),
				Details: map[string]interface{}{
					"correlation_id": correlationID,
				},
			})
			return
		}
	}

	// // Converter entidade para DTO de resposta
	responseDTO := h.mapEntityToResponse(createdEnvelope, createdSignatories)

	// // Log da persistência dos dados brutos do Clicksign
	// // rawDataPersisted := createdEnvelope.ClicksignRawData != nil
	// // var rawDataSize int
	// // if rawDataPersisted {
	// // 	rawDataSize = len(*createdEnvelope.ClicksignRawData)
	// // }

	c.JSON(http.StatusCreated, responseDTO)
}

// @Summary Get envelope
// @Description Get envelope by ID. The response includes clicksign_raw_data field with the complete JSON response from Clicksign API when available (optional field for debugging and analysis).
// @Tags envelopes
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param id path int true "Envelope ID"
// @Success 200 {object} dtos.EnvelopeResponseDTO "Envelope data with optional clicksign_raw_data field containing raw Clicksign API response"
// @Failure 404 {object} dtos.ErrorResponseDTO
// @Failure 500 {object} dtos.ErrorResponseDTO
// @Router /api/v1/envelopes/{id} [get]
func (h *EnvelopeHandlers) GetEnvelopeHandler(c *gin.Context) {
	_ = c.GetHeader("X-Correlation-ID")

	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, dtos.ErrorResponseDTO{
			Error:   "Invalid ID",
			Message: "Envelope ID must be a valid integer",
		})
		return
	}

	envelope, err := h.UsecaseEnvelope.GetEnvelope(id)
	if err != nil {
		c.JSON(http.StatusNotFound, dtos.ErrorResponseDTO{
			Error:   "Envelope not found",
			Message: "The requested envelope does not exist",
		})
		return
	}

	responseDTO := h.mapEntityToResponse(envelope)

	c.JSON(http.StatusOK, responseDTO)
}

// @Summary List envelopes
// @Description Get list of envelopes with optional filters
// @Tags envelopes
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param search query string false "Search term"
// @Param status query string false "Status filter"
// @Param clicksign_key query string false "Clicksign key filter"
// @Success 200 {object} dtos.EnvelopeListResponseDTO
// @Failure 500 {object} dtos.ErrorResponseDTO
// @Router /api/v1/envelopes [get]
func (h *EnvelopeHandlers) GetEnvelopesHandler(c *gin.Context) {
	_ = c.GetHeader("X-Correlation-ID")

	var filters entity.EntityEnvelopeFilters
	filters.Search = c.Query("search")
	filters.Status = c.Query("status")
	filters.ClicksignKey = c.Query("clicksign_key")

	envelopes, err := h.UsecaseEnvelope.GetEnvelopes(filters)
	if err != nil {
		c.JSON(http.StatusInternalServerError, dtos.ErrorResponseDTO{
			Error:   "Internal server error",
			Message: "Failed to retrieve envelopes",
		})
		return
	}

	responseDTO := h.mapEnvelopeListToResponse(envelopes)

	c.JSON(http.StatusOK, responseDTO)
}

// @Summary Activate envelope
// @Description Activate envelope to start signing process
// @Tags envelopes
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param id path int true "Envelope ID"
// @Success 200 {object} dtos.EnvelopeResponseDTO
// @Failure 400 {object} dtos.ErrorResponseDTO
// @Failure 404 {object} dtos.ErrorResponseDTO
// @Failure 500 {object} dtos.ErrorResponseDTO
// @Router /api/v1/envelopes/{id}/activate [post]
func (h *EnvelopeHandlers) ActivateEnvelopeHandler(c *gin.Context) {
	correlationID := c.GetHeader("X-Correlation-ID")
	if correlationID == "" {
		correlationID = strconv.FormatInt(time.Now().Unix(), 10)
	}

	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, dtos.ErrorResponseDTO{
			Error:   "Invalid ID",
			Message: "Envelope ID must be a valid integer",
		})
		return
	}

	envelope, err := h.UsecaseEnvelope.ActivateEnvelope(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, dtos.ErrorResponseDTO{
			Error:   "Internal server error",
			Message: "Failed to activate envelope",
			Details: map[string]interface{}{
				"correlation_id": correlationID,
			},
		})
		return
	}

	responseDTO := h.mapEntityToResponse(envelope)

	c.JSON(http.StatusOK, responseDTO)
}

// @Summary Notify envelope
// @Description Send notification to envelope signatories
// @Tags envelopes
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param id path int true "Envelope ID"
// @Param request body dtos.EnvelopeNotificationRequestDTO true "Notification data"
// @Success 200 {object} dtos.EnvelopeNotificationResponseDTO
// @Failure 400 {object} dtos.ErrorResponseDTO
// @Failure 404 {object} dtos.ErrorResponseDTO
// @Failure 500 {object} dtos.ErrorResponseDTO
// @Router /api/v1/envelopes/{id}/notify [post]
func (h *EnvelopeHandlers) NotifyEnvelopeHandler(c *gin.Context) {
	correlationID := c.GetHeader("X-Correlation-ID")
	if correlationID == "" {
		correlationID = strconv.FormatInt(time.Now().Unix(), 10)
	}

	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, dtos.ErrorResponseDTO{
			Error:   "Invalid ID",
			Message: "Envelope ID must be a valid integer",
		})
		return
	}

	var requestDTO dtos.EnvelopeNotificationRequestDTO

	if err := c.ShouldBindJSON(&requestDTO); err != nil {
		validationErrors := h.extractValidationErrors(err)
		c.JSON(http.StatusBadRequest, dtos.ValidationErrorResponseDTO{
			Error:   "Validation failed",
			Message: "Invalid request payload",
			Details: validationErrors,
		})
		return
	}

	// Enviar notificação através do use case
	err = h.UsecaseEnvelope.NotifyEnvelope(c.Request.Context(), id, requestDTO.Message)
	if err != nil {
		c.JSON(http.StatusInternalServerError, dtos.ErrorResponseDTO{
			Error:   "Internal server error",
			Message: "Failed to send notification: " + err.Error(),
			Details: map[string]interface{}{
				"correlation_id": correlationID,
			},
		})
		return
	}

	responseDTO := dtos.EnvelopeNotificationResponseDTO{
		Success: true,
		Message: "Notification sent successfully",
	}

	c.JSON(http.StatusOK, responseDTO)
}

// Helper methods

func (h *EnvelopeHandlers) mapCreateRequestToEntity(dto dtos.EnvelopeCreateRequestDTO) (*entity.EntityEnvelope, []*entity.EntityDocument, error) {
	// Determinar emails dos signatários com base no formato usado
	var signatoryEmails []string
	if len(dto.SignatoryEmails) > 0 {
		// Usando formato antigo com emails diretos
		signatoryEmails = dto.SignatoryEmails
	} else if len(dto.Signatories) > 0 {
		// Usando formato novo com signatários estruturados - extrair emails
		for _, signatory := range dto.Signatories {
			signatoryEmails = append(signatoryEmails, signatory.Email)
		}
	}

	envelope := &entity.EntityEnvelope{
		Name:            dto.Name,
		Description:     dto.Description,
		DocumentsIDs:    dto.DocumentsIDs,
		SignatoryEmails: signatoryEmails,
		Message:         dto.Message,
		DeadlineAt:      dto.DeadlineAt,
		RemindInterval:  dto.RemindInterval,
		AutoClose:       dto.AutoClose,
		Status:          "draft",
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}

	if envelope.RemindInterval == 0 {
		envelope.RemindInterval = 3
	}

	var documents []*entity.EntityDocument

	// Processar documentos base64 se fornecidos
	for _, docRequest := range dto.Documents {
		// Processar base64
		fileInfo, err := utils.DecodeBase64File(docRequest.FileContentBase64)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to process base64 content for document '%s': %w", docRequest.Name, err)
		}

		// Validar MIME type
		if err := utils.ValidateMimeType(fileInfo.MimeType); err != nil {
			utils.CleanupTempFile(fileInfo.TempPath)
			return nil, nil, fmt.Errorf("unsupported file type for document '%s': %w", docRequest.Name, err)
		}

		document := &entity.EntityDocument{
			Name:         docRequest.Name,
			Description:  docRequest.Description,
			FilePath:     fileInfo.TempPath,
			FileSize:     fileInfo.Size,
			MimeType:     fileInfo.MimeType,
			IsFromBase64: true,
			Status:       "draft",
		}

		documents = append(documents, document)
	}

	return envelope, documents, nil
}

func (h *EnvelopeHandlers) mapEntityToResponse(envelope *entity.EntityEnvelope, signatories ...[]entity.EntitySignatory) *dtos.EnvelopeResponseDTO {
	response := &dtos.EnvelopeResponseDTO{
		ID:               envelope.ID,
		Name:             envelope.Name,
		Description:      envelope.Description,
		Status:           envelope.Status,
		ClicksignKey:     envelope.ClicksignKey,
		ClicksignRawData: envelope.ClicksignRawData,
		DocumentsIDs:     envelope.DocumentsIDs,
		SignatoryEmails:  envelope.SignatoryEmails,
		Message:          envelope.Message,
		DeadlineAt:       envelope.DeadlineAt,
		RemindInterval:   envelope.RemindInterval,
		AutoClose:        envelope.AutoClose,
		CreatedAt:        envelope.CreatedAt,
		UpdatedAt:        envelope.UpdatedAt,
	}

	// Incluir signatários se fornecidos
	if len(signatories) > 0 && len(signatories[0]) > 0 {
		signatoryDTOs := make([]dtos.SignatoryResponseDTO, len(signatories[0]))
		for i, signatory := range signatories[0] {
			signatoryDTOs[i].FromEntity(&signatory)
		}
		response.Signatories = signatoryDTOs
	}

	return response
}

func (h *EnvelopeHandlers) mapEnvelopeListToResponse(envelopes []entity.EntityEnvelope) *dtos.EnvelopeListResponseDTO {
	envelopeList := make([]dtos.EnvelopeResponseDTO, len(envelopes))
	for i, envelope := range envelopes {
		envelopeList[i] = *h.mapEntityToResponse(&envelope)
	}

	return &dtos.EnvelopeListResponseDTO{
		Envelopes: envelopeList,
		Total:     len(envelopes),
	}
}

func (h *EnvelopeHandlers) extractValidationErrors(err error) []dtos.ValidationErrorDetail {
	var validationErrors []dtos.ValidationErrorDetail

	if validationErr, ok := err.(validator.ValidationErrors); ok {
		for _, fieldError := range validationErr {
			validationErrors = append(validationErrors, dtos.ValidationErrorDetail{
				Field:   fieldError.Field(),
				Message: h.getValidationErrorMessage(fieldError),
				Value:   fmt.Sprintf("%v", fieldError.Value()),
			})
		}
	} else {
		validationErrors = append(validationErrors, dtos.ValidationErrorDetail{
			Field:   "general",
			Message: err.Error(),
		})
	}

	return validationErrors
}

func (h *EnvelopeHandlers) getValidationErrorMessage(fieldError validator.FieldError) string {
	switch fieldError.Tag() {
	case "required":
		return "This field is required"
	case "min":
		return "This field must have at least " + fieldError.Param() + " characters/items"
	case "max":
		return "This field must have at most " + fieldError.Param() + " characters/items"
	case "email":
		return "This field must be a valid email address"
	default:
		return "This field is invalid"
	}
}

// @Summary Check signature events manually (webhook fallback)
// @Description Fallback endpoint that checks Clicksign events API when webhooks fail. Processes sign events and triggers internal webhooks to maintain existing workflow.
// @Tags envelopes
// @Accept json
// @Produce json
// @Security ApiKeyAuth
// @Param id path int true "Envelope ID"
// @Success 200 {object} dtos.WebhookProcessResponseDTO
// @Failure 400 {object} dtos.ErrorResponseDTO
// @Failure 404 {object} dtos.ErrorResponseDTO
// @Failure 500 {object} dtos.ErrorResponseDTO
// @Router /api/v1/envelopes/{id}/events/check [post]
func (h *EnvelopeHandlers) CheckSignatureEventsHandler(c *gin.Context) {
	idParam := c.Param("id")
	envelopeID, err := strconv.Atoi(idParam)
	if err != nil {
		c.JSON(http.StatusBadRequest, dtos.ErrorResponseDTO{
			Error:   "validation_error",
			Message: "Invalid envelope ID",
		})
		return
	}

	h.Logger.WithField("envelope_id", envelopeID).Info("Manual check: fetching signature events from Clicksign API")

	// Usar usecase para buscar eventos
	eventsResult, err := h.UsecaseEnvelope.CheckEventsFromClicksignAPI(c.Request.Context(), envelopeID)
	if err != nil {
		h.Logger.WithError(err).WithField("envelope_id", envelopeID).Error("Failed to check events from Clicksign API")

		// Determinar tipo de erro para resposta apropriada
		statusCode := http.StatusInternalServerError
		errorType := "internal_error"

		if err.Error() == "envelope not found" {
			statusCode = http.StatusNotFound
			errorType = "not_found_error"
		} else if err.Error() == "envelope does not have clicksign_key" {
			statusCode = http.StatusBadRequest
			errorType = "validation_error"
		}

		c.JSON(statusCode, dtos.ErrorResponseDTO{
			Error:   errorType,
			Message: err.Error(),
		})
		return
	}

	// Processar os eventos encontrados via webhooks
	processedEvents := 0
	for _, event := range eventsResult.Events {
		// Criar webhook DTO simulando evento de assinatura
		webhookDTO := &dtos.WebhookRequestDTO{
			Event: dtos.WebhookEventDTO{
				Name:       "sign",
				OccurredAt: fmt.Sprintf("%v", event.SignedAt), // Converter para string
				Data: map[string]interface{}{
					"signer": map[string]interface{}{
						"key":   event.SignerKey,
						"email": event.Email,
						"name":  event.Name,
					},
				},
			},
			Document: dtos.WebhookDocumentDTO{
				Key:        eventsResult.EnvelopeKey,
				AccountKey: "api-fallback",
				Status:     "running",
			},
		}

		// Processar evento via webhook usecase
		rawPayload := fmt.Sprintf(`{"source":"api_fallback","signer_key":"%s","envelope_id":%d}`,
			event.SignerKey, envelopeID)

		_, err := h.UsecaseWebhook.ProcessWebhook(webhookDTO, rawPayload)
		if err != nil {
			h.Logger.WithError(err).WithFields(logrus.Fields{
				"signer_key":  event.SignerKey,
				"envelope_id": envelopeID,
			}).Error("Failed to process signature event via webhook")
			continue
		}

		processedEvents++
		h.Logger.WithFields(logrus.Fields{
			"signer_key":  event.SignerKey,
			"envelope_id": envelopeID,
			"email":       event.Email,
		}).Info("Successfully processed signature event via webhook")
	}

	// Montar resposta
	message := fmt.Sprintf("Checked Clicksign API: found %d events, processed %d via webhooks",
		len(eventsResult.Events), processedEvents)

	response := &dtos.WebhookProcessResponseDTO{
		Success: true,
		Message: message,
	}

	h.Logger.WithFields(logrus.Fields{
		"envelope_id":      envelopeID,
		"events_found":     len(eventsResult.Events),
		"events_processed": processedEvents,
	}).Info("Manual event check completed successfully")

	c.JSON(http.StatusOK, response)
}

func MountEnvelopeHandlers(gin *gin.Engine, conn *gorm.DB, logger *logrus.Logger) {
	clicksignClient := clicksign.NewClicksignClient(config.EnvironmentVariables, logger)

	// Criar usecase de documento para envelopes com documentos base64
	usecaseDocument := document.NewUsecaseDocumentServiceWithClicksign(
		repository.NewRepositoryDocument(conn),
		clicksignClient,
		logger,
	)

	// Criar usecase de signatory
	usecaseSignatory := signatory.NewUsecaseSignatoryService(
		repository.NewRepositorySignatory(conn),
		repository.NewRepositoryEnvelope(conn),
		clicksignClient,
		logger,
	)

	// Criar usecase de requirement
	usecaseRequirement := requirement.NewUsecaseRequirementService(
		repository.NewRepositoryRequirement(conn),
		repository.NewRepositoryEnvelope(conn),
		clicksignClient,
		logger,
	)

	envelopeUsecase := usecase_envelope.NewUsecaseEnvelopeService(
		repository.NewRepositoryEnvelope(conn),
		clicksignClient,
		usecaseDocument,
		usecaseRequirement,
		logger,
	)

	// // Criar usecase de webhook
	usecaseWebhook := webhook.NewUsecaseWebhookService(
		repository.NewRepositoryWebhook(conn),
		envelopeUsecase,
		usecaseDocument,
		logger,
	)

	envelopeHandlers := NewEnvelopeHandler(
		envelopeUsecase,
		usecaseDocument,
		usecaseRequirement,
		usecaseSignatory,
		logger,
	)

	// Injetar webhook usecase no handler
	envelopeHandlers.UsecaseWebhook = usecaseWebhook

	// Criar handler de requirements
	requirementHandlers := NewRequirementHandler(usecaseRequirement, logger)

	group := gin.Group("/api/v1/envelopes")
	SetAuthMiddleware(conn, group)

	group.POST("/", envelopeHandlers.CreateEnvelopeHandler)
	group.GET("/:id", envelopeHandlers.GetEnvelopeHandler)
	group.GET("/", envelopeHandlers.GetEnvelopesHandler)
	group.POST("/:id/activate", envelopeHandlers.ActivateEnvelopeHandler)
	group.POST("/:id/notify", envelopeHandlers.NotifyEnvelopeHandler)

	// Rota de fallback para verificar eventos manualmente quando webhook falha
	group.POST("/:id/events/check", envelopeHandlers.CheckSignatureEventsHandler)

	// Rotas de requirements por envelope
	group.POST("/:id/requirements", requirementHandlers.CreateRequirementHandler)
	group.GET("/:id/requirements", requirementHandlers.GetRequirementsByEnvelopeHandler)
}
