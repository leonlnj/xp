package services

import (
	"fmt"
	"strings"
	"time"

	"github.com/caraml-dev/xp/management-service/utils"
	"github.com/golang-collections/collections/set"
	"golang.org/x/sync/errgroup"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/caraml-dev/xp/management-service/errors"
	"github.com/caraml-dev/xp/management-service/models"
	"github.com/caraml-dev/xp/management-service/pagination"
)

type ExperimentStatusFriendly string

// Defines values for ExperimentStatusFriendly.
const (
	ExperimentStatusFriendlyCompleted   ExperimentStatusFriendly = "completed"
	ExperimentStatusFriendlyDeactivated ExperimentStatusFriendly = "deactivated"
	ExperimentStatusFriendlyRunning     ExperimentStatusFriendly = "running"
	ExperimentStatusFriendlyScheduled   ExperimentStatusFriendly = "scheduled"
)

type CreateExperimentRequestBody struct {
	Description *string                     `json:"description"`
	EndTime     time.Time                   `json:"end_time" validate:"required,gtfield=StartTime"`
	Interval    *int32                      `json:"interval"`
	Name        string                      `json:"name" validate:"required,notBlank"`
	Segment     models.ExperimentSegmentRaw `json:"segment"`
	StartTime   time.Time                   `json:"start_time" validate:"required"`
	Status      models.ExperimentStatus     `json:"status" validate:"required,oneof=inactive active"`
	Treatments  models.ExperimentTreatments `json:"treatments" validate:"unique=Name,dive,required,notBlank"`
	Tier        models.ExperimentTier       `json:"tier" validate:"required,oneof=default override"`
	Type        models.ExperimentType       `json:"type" validate:"required,oneof=A/B Switchback"`
	UpdatedBy   *string                     `json:"updated_by,omitempty"`
}

type UpdateExperimentRequestBody struct {
	Description *string                     `json:"description"`
	EndTime     time.Time                   `json:"end_time" validate:"required,gtfield=StartTime"`
	Interval    *int32                      `json:"interval"`
	Segment     models.ExperimentSegmentRaw `json:"segment"`
	StartTime   time.Time                   `json:"start_time" validate:"required"`
	Status      models.ExperimentStatus     `json:"status" validate:"required,oneof=inactive active"`
	Treatments  models.ExperimentTreatments `json:"treatments" validate:"unique=Name,dive,required,notBlank"`
	Tier        models.ExperimentTier       `json:"tier" validate:"required,oneof=default override"`
	Type        models.ExperimentType       `json:"type" validate:"required,oneof=A/B Switchback"`
	UpdatedBy   *string                     `json:"updated_by,omitempty"`
}

type ListExperimentsParams struct {
	pagination.PaginationOptions
	Status           *models.ExperimentStatus   `json:"status,omitempty"`
	StatusFriendly   []ExperimentStatusFriendly `json:"status_friendly"`
	EndTime          *time.Time                 `json:"end_time,omitempty"`
	Tier             *models.ExperimentTier     `json:"tier,omitempty"`
	Type             *models.ExperimentType     `json:"type,omitempty"`
	Name             *string                    `json:"name,omitempty"`
	UpdatedBy        *string                    `json:"updated_by,omitempty"`
	Search           *string                    `json:"search,omitempty"`
	StartTime        *time.Time                 `json:"start_time,omitempty"`
	Segment          models.ExperimentSegment   `json:"segment,omitempty"`
	IncludeWeakMatch bool                       `json:"include_weak_match"`
	Fields           *[]models.ExperimentField  `json:"fields,omitempty"`
}

type ExperimentService interface {
	ListExperiments(
		projectId int64,
		params ListExperimentsParams,
	) ([]*models.Experiment, *pagination.Paging, error)
	ListAllExperiments(projectId models.ID, params ListExperimentsParams) ([]*models.Experiment, error)
	GetExperiment(projectId int64, experimentId int64) (*models.Experiment, error)
	CreateExperiment(settings models.Settings, expData CreateExperimentRequestBody) (*models.Experiment, error)
	UpdateExperiment(settings models.Settings, experimentId int64, expData UpdateExperimentRequestBody) (*models.Experiment, error)
	EnableExperiment(settings models.Settings, experimentId int64) error
	DisableExperiment(projectId int64, experimentId int64) error
	ValidatePairwiseExperimentOrthogonality(projectId int64, experiments []*models.Experiment, segmenters []string) error
	ValidateProjectExperimentSegmentersExist(projectId int64, experiments []*models.Experiment, segmenters []string) error

	GetDBRecord(projectId models.ID, experimentId models.ID) (*models.Experiment, error)
	RunCustomValidation(
		experiment models.Experiment,
		settings models.Settings,
		context ValidationContext,
		operationType OperationType,
	) error
}

type experimentService struct {
	services *Services
	db       *gorm.DB
}

func NewExperimentService(
	services *Services,
	db *gorm.DB,
) ExperimentService {
	return &experimentService{
		services: services,
		db:       db,
	}
}

func (svc *experimentService) ListExperiments(
	projectId int64,
	params ListExperimentsParams,
) ([]*models.Experiment, *pagination.Paging, error) {
	var err error
	var exps []*models.Experiment

	query := svc.query()

	// Handle Field values
	query, err = svc.filterFieldValues(query, params)
	if err != nil {
		return nil, nil, err
	}

	query = query.
		Where("project_id = ?", projectId).
		Order("updated_at desc")

	// Handle optional parameters
	if params.Status != nil {
		query = query.Where("status = ?", params.Status)
	}
	// Handle StatusFriendly values
	if len(params.StatusFriendly) > 0 {
		query = svc.filterExperimentStatusFriendly(query, params.StatusFriendly)
	}

	// Handle Start and EndTime values
	query, err = svc.filterStartEndTimeValues(query, params)
	if err != nil {
		return nil, nil, err
	}

	if params.Tier != nil {
		query = query.Where("tier = ?", params.Tier)
	}
	if params.Type != nil {
		query = query.Where("type = ?", params.Type)
	}
	if params.Name != nil {
		query = query.Where("name = ?", params.Name)
	}
	if params.UpdatedBy != nil {
		query = query.Where(
			fmt.Sprintf("updated_by ILIKE '%%%s%%'", *params.UpdatedBy),
		)
	}
	if params.Search != nil {
		query = query.Where(
			fmt.Sprintf("name ILIKE '%%%s%%' OR description ILIKE '%%%s%%'", *params.Search, *params.Search),
		)
	}
	// Segmenters
	query = svc.filterSegmenterValues(query, params.Segment, params.IncludeWeakMatch)

	// Pagination
	var pagingResponse *pagination.Paging
	var count int64
	if params.Fields == nil || params.Page != nil || params.PageSize != nil {
		err = pagination.ValidatePaginationParams(params.Page, params.PageSize)
		if err != nil {
			return nil, nil, err
		}
		pageOpts := pagination.NewPaginationOptions(params.Page, params.PageSize)
		// Count total
		query.Model(&exps).Count(&count)
		// Add offset and limit
		query = query.Offset(int((*pageOpts.Page - 1) * *pageOpts.PageSize))
		query = query.Limit(int(*pageOpts.PageSize))
		// Format opts into paging response
		pagingResponse = pagination.ToPaging(pageOpts, int(count))
		if pagingResponse.Page > 1 && pagingResponse.Pages < pagingResponse.Page {
			// Invalid query - total pages is less than the requested page
			return nil, nil, errors.Newf(errors.BadInput,
				"Requested page number %d exceeds total pages: %d.", pagingResponse.Page, pagingResponse.Pages)
		}
	}

	// Filter experiments
	err = query.Find(&exps).Error
	if err != nil {
		return nil, nil, err
	}

	return exps, pagingResponse, nil
}

func (svc *experimentService) GetExperiment(projectId int64, experimentId int64) (*models.Experiment, error) {
	exp, err := svc.GetDBRecord(models.ID(projectId), models.ID(experimentId))
	if err != nil {
		return nil, errors.Newf(errors.NotFound, err.Error())
	}

	return exp, nil
}

func (svc *experimentService) CreateExperiment(
	settings models.Settings,
	expData CreateExperimentRequestBody,
) (*models.Experiment, error) {
	// Validate experiment data
	err := svc.services.ValidationService.Validate(expData)
	if err != nil {
		return nil, errors.Newf(errors.BadInput, err.Error())
	}

	// Validate Segmenter data
	err = svc.services.SegmenterService.ValidateExperimentSegment(
		int64(settings.ProjectID),
		settings.Config.Segmenters.Names,
		expData.Segment,
	)
	if err != nil {
		return nil, errors.Newf(errors.BadInput, err.Error())
	}

	// If new experiment is active, get other experiments active in the same time range
	// and validate segment orthogonality
	if expData.Status == models.ExperimentStatusActive {
		err = svc.validateExperimentOrthogonalityInDuration(nil, settings, expData.Segment, expData.Tier, expData.StartTime, expData.EndTime)
		if err != nil {
			return nil, err
		}

		// Check if the set of segmenters contains all the segments specified by the experiment
		err = validateExperimentSegmentersExist(
			expData.Name,
			expData.Segment,
			utils.StringSliceToSet(settings.Config.Segmenters.Names),
		)
		if err != nil {
			return nil, err
		}
	}

	segmenterTypes, err := svc.services.SegmenterService.GetSegmenterTypes(int64(settings.ProjectID))
	if err != nil {
		return nil, err
	}
	segmenterStorageSchema, err := expData.Segment.ToStorageSchema(segmenterTypes)
	if err != nil {
		return nil, err
	}
	// Create the experiment record
	experiment := &models.Experiment{
		ProjectID:   settings.ProjectID,
		Name:        expData.Name,
		Description: expData.Description,
		Tier:        expData.Tier,
		Type:        expData.Type,
		Interval:    expData.Interval,
		Treatments:  expData.Treatments,
		Segment:     segmenterStorageSchema,
		Status:      expData.Status,
		StartTime:   expData.StartTime,
		EndTime:     expData.EndTime,
		UpdatedBy:   *expData.UpdatedBy,
		Version:     1,
	}

	// Validate the experiment against the project settings' treatment schema and validation url
	err = svc.RunCustomValidation(
		*experiment,
		settings,
		ValidationContext{},
		OperationTypeCreate,
	)
	if err != nil {
		return nil, errors.Newf(errors.BadInput, err.Error())
	}

	// Save to DB
	expDBRecord, err := svc.save(experiment)
	if err != nil {
		return nil, err
	}

	// Convert to the format expected by the Message Queue
	protoExpResponse, err := expDBRecord.ToProtoSchema(segmenterTypes)
	if err != nil {
		return nil, err
	}
	err = svc.services.PubSubPublisherService.PublishExperimentMessage("create", protoExpResponse)
	if err != nil {
		return nil, err
	}

	return expDBRecord, nil
}

func (svc *experimentService) UpdateExperiment(
	settings models.Settings,
	experimentId int64,
	expData UpdateExperimentRequestBody,
) (*models.Experiment, error) {
	// Validate experiment data
	err := svc.services.ValidationService.Validate(expData)
	if err != nil {
		return nil, errors.Newf(errors.BadInput, err.Error())
	}

	err = svc.services.SegmenterService.ValidateExperimentSegment(
		int64(settings.ProjectID),
		settings.Config.Segmenters.Names,
		expData.Segment,
	)
	if err != nil {
		return nil, errors.Newf(errors.BadInput, err.Error())
	}

	// Get current experiment
	curExperiment, err := svc.GetDBRecord(settings.ProjectID, models.ID(experimentId))
	if err != nil {
		return nil, err
	}

	// If new experiment is active, get other experiments active in the same time range
	// and validate segment orthogonality
	if expData.Status == models.ExperimentStatusActive {
		err = svc.validateExperimentOrthogonalityInDuration(&experimentId, settings, expData.Segment, expData.Tier, expData.StartTime, expData.EndTime)
		if err != nil {
			return nil, err
		}

		// Check if the set of segmenters contains all the segments specified by the experiment
		err = validateExperimentSegmentersExist(
			curExperiment.Name,
			expData.Segment,
			utils.StringSliceToSet(settings.Config.Segmenters.Names),
		)
		if err != nil {
			return nil, err
		}
	}

	// Validate experiment type
	if expData.Type != curExperiment.Type {
		return nil, errors.Newf(errors.BadInput, "experiment type cannot be changed")
	}

	//  Copy current experiment's contents as experiment history
	_, err = svc.services.ExperimentHistoryService.CreateExperimentHistory(curExperiment)
	if err != nil {
		return nil, err
	}

	segmenterTypes, err := svc.services.SegmenterService.GetSegmenterTypes(int64(settings.ProjectID))
	if err != nil {
		return nil, err
	}
	segmenterStorageSchema, err := expData.Segment.ToStorageSchema(segmenterTypes)
	if err != nil {
		return nil, err
	}
	newExperiment := &models.Experiment{
		// Copy the ID and the fixed fields
		ID:        curExperiment.ID,
		ProjectID: curExperiment.ProjectID,
		Name:      curExperiment.Name,
		Type:      curExperiment.Type,
		// Increment the version
		Version: curExperiment.Version + 1,
		// Add the new data
		Description: expData.Description,
		Interval:    expData.Interval,
		Treatments:  expData.Treatments,
		Segment:     segmenterStorageSchema,
		Status:      expData.Status,
		StartTime:   expData.StartTime,
		Tier:        expData.Tier,
		EndTime:     expData.EndTime,
		UpdatedBy:   *expData.UpdatedBy,
	}

	// Validate the experiment against the project settings' treatment schema and validation url
	err = svc.RunCustomValidation(
		*newExperiment,
		settings,
		ValidationContext{CurrentData: curExperiment},
		OperationTypeUpdate,
	)
	if err != nil {
		return nil, errors.Newf(errors.BadInput, err.Error())
	}

	// Update current experiment and save to DB
	expDBRecord, err := svc.save(newExperiment)
	if err != nil {
		return nil, err
	}

	// Publish pubsub update message
	protoExpResponse, err := expDBRecord.ToProtoSchema(segmenterTypes)
	if err != nil {
		return nil, err
	}
	err = svc.services.PubSubPublisherService.PublishExperimentMessage("update", protoExpResponse)
	if err != nil {
		return nil, err
	}

	return expDBRecord, nil
}

func (svc *experimentService) EnableExperiment(settings models.Settings, experimentId int64) error {
	// Get experiment
	experiment, err := svc.GetDBRecord(settings.ProjectID, models.ID(experimentId))
	if err != nil {
		return err
	}

	// Experiment is already active
	if experiment.Status == models.ExperimentStatusActive {
		return errors.Newf(errors.BadInput, fmt.Sprintf("experiment id %d is already active", experimentId))
	}

	segmenterTypes, err := svc.services.SegmenterService.GetSegmenterTypes(int64(settings.ProjectID))
	if err != nil {
		return err
	}
	// Validate that the experiment has all the required segmenters activated
	rawSegments, err := experiment.Segment.ToRawSchema(segmenterTypes)
	if err != nil {
		return err
	}
	// Check if the set of segmenters contains all the segments specified by the experiment
	err = validateExperimentSegmentersExist(
		experiment.Name,
		rawSegments,
		utils.StringSliceToSet(settings.Config.Segmenters.Names),
	)
	if err != nil {
		return errors.Newf(
			errors.BadInput,
			fmt.Sprintf("Error validating segmenters required for enabling experiment: %s", err.Error()),
		)
	}

	// Get other experiments active in the same time range and validate segment orthogonality
	rawSegments, err = experiment.Segment.ToRawSchema(segmenterTypes)
	if err != nil {
		return err
	}

	err = svc.validateExperimentOrthogonalityInDuration(&experimentId, settings,
		rawSegments, experiment.Tier, experiment.StartTime, experiment.EndTime)
	if err != nil {
		return err
	}

	//  Copy current experiment's contents as experiment history
	_, err = svc.services.ExperimentHistoryService.CreateExperimentHistory(experiment)
	if err != nil {
		return err
	}

	// Update Experiment
	experiment.Status = models.ExperimentStatusActive
	expDBRecord, err := svc.save(experiment)
	if err != nil {
		return err
	}

	// Convert to the format expected by the Message Queue
	protoExpResponse, err := expDBRecord.ToProtoSchema(segmenterTypes)
	if err != nil {
		return err
	}
	err = svc.services.PubSubPublisherService.PublishExperimentMessage("update", protoExpResponse)
	if err != nil {
		return err
	}

	return nil
}

func (svc *experimentService) DisableExperiment(projectId int64, experimentId int64) error {
	// Get experiment
	experiment, err := svc.GetDBRecord(models.ID(projectId), models.ID(experimentId))
	if err != nil {
		return err
	}

	// Experiment is already inactive
	if experiment.Status == models.ExperimentStatusInactive {
		return errors.Newf(errors.BadInput, fmt.Sprintf("experiment id %d is already inactive", experimentId))
	}

	//  Copy current experiment's contents as experiment history
	_, err = svc.services.ExperimentHistoryService.CreateExperimentHistory(experiment)
	if err != nil {
		return err
	}

	// Update Experiment
	experiment.Status = models.ExperimentStatusInactive
	expDBRecord, err := svc.save(experiment)
	if err != nil {
		return err
	}

	// Convert to the format expected by the Message Queue
	segmenterTypes, err := svc.services.SegmenterService.GetSegmenterTypes(projectId)
	if err != nil {
		return err
	}
	protoExpResponse, err := expDBRecord.ToProtoSchema(segmenterTypes)
	if err != nil {
		return err
	}
	err = svc.services.PubSubPublisherService.PublishExperimentMessage("update", protoExpResponse)
	if err != nil {
		return err
	}

	return nil
}

func (svc *experimentService) GetDBRecord(projectId models.ID, experimentId models.ID) (*models.Experiment, error) {
	var exp models.Experiment
	query := svc.query().
		Where("project_id = ?", projectId).
		Where("id = ?", experimentId).
		First(&exp)
	if err := query.Error; err != nil {
		return nil, err
	}
	return &exp, nil
}

func (svc *experimentService) query() *gorm.DB {
	return svc.db
}

func (svc *experimentService) save(exp *models.Experiment) (*models.Experiment, error) {
	if err := svc.query().Clauses(clause.OnConflict{
		UpdateAll: true,
	}).Create(exp).Error; err != nil {
		return nil, err
	}
	return svc.GetDBRecord(exp.ProjectID, exp.ID)
}

func (svc *experimentService) filterFieldValues(query *gorm.DB, params ListExperimentsParams) (*gorm.DB, error) {
	if params.Fields != nil && len(*params.Fields) != 0 {
		err := validateListExperimentFieldNames(*params.Fields)
		if err != nil {
			return nil, err
		}

		// Stores a set of unique field names which are unique column names to be selected in the db query
		fieldNamesSet := set.New()
		for _, field := range *params.Fields {
			fieldName := field
			// Add ExperimentFieldStatus to the query because status_friendly does not exist in the db as a column
			if field == models.ExperimentFieldStatusFriendly {
				// Add ExperimentFieldStartTime and ExperimentFieldEndTime to the query as they are required for
				// determining the statusFriendly field; these two fields may or may not be returned depending on
				// what is actually specified in params.Fields
				fieldNamesSet.Insert(string(models.ExperimentFieldStartTime))
				fieldNamesSet.Insert(string(models.ExperimentFieldEndTime))

				fieldName = models.ExperimentFieldStatus
			}
			//fieldNamesSet[string(fieldName)] = struct{}{}
			fieldNamesSet.Insert(string(fieldName))
		}

		// Retrieve a slice of unique strings from fieldNamesSet; query.Select only accepts []string
		var fieldNames []string
		fieldNamesSet.Do(func(fieldName interface{}) {
			fieldNames = append(fieldNames, fmt.Sprint(fieldName))
		})

		query = query.Select(fieldNames)
	}
	return query, nil
}

func (svc *experimentService) filterExperimentStatusFriendly(query *gorm.DB, statusesFriendly []ExperimentStatusFriendly) *gorm.DB {
	orPredicates := svc.query().Where("false") // start with false and build OR query dynamically
	for _, statusFriendly := range statusesFriendly {
		predicates := svc.query()
		if statusFriendly == ExperimentStatusFriendlyDeactivated {
			predicates = predicates.Where("status = ?", models.ExperimentStatusInactive)
		} else {
			predicates = predicates.Where("status = ?", models.ExperimentStatusActive)
			switch statusFriendly {
			case ExperimentStatusFriendlyScheduled:
				predicates = predicates.Where("start_time > current_timestamp")
			case ExperimentStatusFriendlyCompleted:
				predicates = predicates.Where("end_time < current_timestamp")
			default:
				// Status Running - current time should be present in the experiment duration.
				predicates = predicates.Where("tstzrange(start_time, end_time, '[)') @> tstzrange(current_timestamp, current_timestamp, '[]')")
			}
		}
		orPredicates = orPredicates.Or(predicates)
	}
	return query.Where(orPredicates)
}

func (svc *experimentService) filterStartEndTimeValues(query *gorm.DB, params ListExperimentsParams) (*gorm.DB, error) {
	if params.StartTime != nil && !params.StartTime.IsZero() && (params.EndTime == nil || params.EndTime.IsZero()) {
		return nil, errors.Newf(errors.BadInput, "end_time parameter must be supplied as well")
	}
	if params.EndTime != nil && !params.EndTime.IsZero() && (params.StartTime == nil || params.StartTime.IsZero()) {
		return nil, errors.Newf(errors.BadInput, "start_time parameter must be supplied as well")
	}
	if params.StartTime != nil && !params.StartTime.IsZero() && params.EndTime != nil && !params.EndTime.IsZero() {
		// Find experiments that are at least partially running in this window.
		if params.StartTime.Equal(*params.EndTime) {
			// To filter active experiments at a given timestamp (such as current timestamp),
			// it needs to be passed in for both the start and end time.
			query = query.Where("tstzrange(start_time, end_time, '[)') @> tstzrange(?, ?, '[]')", params.StartTime, params.EndTime)
		} else {
			// One of the following should match:
			// * the start_time parameter should fall within the experiment's [start and end) times
			// * the end_time parameter should fall within the experiment's (start and end) times
			// * the experiment starts and ends within the [start_time and end_time) duration
			query = query.Where(
				svc.query().
					Where("tstzrange(start_time, end_time, '[)') @> tstzrange(?, ?, '[]')", params.StartTime, params.StartTime).
					Or("tstzrange(start_time, end_time, '()') @> tstzrange(?, ?, '[]')", params.EndTime, params.EndTime).
					Or("tstzrange(?, ?, '[]') @> tstzrange(start_time, end_time, '[)')", params.StartTime, params.EndTime),
			)
		}
	}
	return query, nil
}

func (svc *experimentService) filterSegmenterValues(query *gorm.DB, segment models.ExperimentSegment, includeWeakMatch bool) *gorm.DB {
	// No need to format the segmenter values according to their types since we're storing all values in string
	for name, values := range segment {
		query = filterSegmenterAnyOfPredicate(query, name, values, includeWeakMatch)
	}
	return query
}

func validateListExperimentFieldNames(fields []models.ExperimentField) error {
	allowedFieldList := []interface{}{
		models.ExperimentFieldId,
		models.ExperimentFieldName,
		models.ExperimentFieldStartTime,
		models.ExperimentFieldEndTime,
		models.ExperimentFieldTier,
		models.ExperimentFieldType,
		models.ExperimentFieldStatusFriendly,
		models.ExperimentFieldUpdatedAt,
		models.ExperimentFieldTreatments,
	}
	allowedFields := set.New(allowedFieldList...)
	for _, field := range fields {
		if !allowedFields.Has(field) {
			return fmt.Errorf("field %s is not supported, fields should only be name and/or id", field)
		}
	}
	return nil
}

func (svc *experimentService) validateExperimentOrthogonality(
	projectId int64,
	experimentId *int64,
	segment models.ExperimentSegmentRaw,
	experiments []*models.Experiment,
	segmenters []string,
) error {
	var err error
	var filteredExps []models.Experiment
	for _, exp := range experiments {
		// Case: Update experiment ONLY; Exclude current experiment id's segments
		if experimentId != nil {
			if exp.ID.ToApiSchema() != *experimentId {
				filteredExps = append(filteredExps, *exp)
			}
			continue
		}
		filteredExps = append(filteredExps, *exp)
	}
	if len(filteredExps) > 0 {
		err = svc.services.SegmenterService.ValidateSegmentOrthogonality(projectId, segmenters, segment, filteredExps)
		if err != nil {
			return errors.Newf(errors.BadInput, err.Error())
		}
	}

	return nil
}

func filterSegmenterAnyOfPredicate(query *gorm.DB, name string, values []string, includeWeakMatch bool) *gorm.DB {
	if len(values) == 0 {
		return query
	}
	// Prepare SQL predicate and add to the query
	matchArray := []string{}
	for _, val := range values {
		matchArray = append(matchArray, fmt.Sprintf("'{\"%s\": [\"%s\"]}'", name, val))
	}
	predicate := fmt.Sprintf("segment @> ANY (ARRAY [%s]::jsonb[])", strings.Join(matchArray, ","))
	// Include weak matches if the flag is set
	if includeWeakMatch {
		predicate = fmt.Sprintf("(%s OR %s OR %s)",
			predicate,
			fmt.Sprintf("NOT (segment ?| '{%v}')", name), // The segment does not exist in the experiment
			fmt.Sprintf("segment-> '%s' = '[]'", name),   // The segment is set as []
		)
	}
	return query.Where(predicate)
}

// ListAllExperiments returns a list of all experiments based on the filters specified in params parameter,
// to be used for performing orthogonality checks on.
func (svc *experimentService) ListAllExperiments(projectId models.ID, params ListExperimentsParams) ([]*models.Experiment, error) {
	// Get the first page of active experiments
	filteredExperiments, paging, err := svc.ListExperiments(
		projectId.ToApiSchema(),
		params,
	)
	if err != nil {
		return nil, err
	}
	if paging == nil {
		// This is not expected (the pagination data should always be set), but handle it.
		return nil, fmt.Errorf("Missing pagination data for existing experiments")
	}

	// If there are multiple pages, get the subsequent pages
	for i := int32(2); i <= paging.Pages; i++ {
		exps, _, err := svc.ListExperiments(
			projectId.ToApiSchema(),
			ListExperimentsParams{
				StartTime: params.StartTime,
				EndTime:   params.EndTime,
				Status:    params.Status,
				PaginationOptions: pagination.PaginationOptions{
					Page: &i,
				},
			},
		)
		if err != nil {
			return nil, err
		}
		filteredExperiments = append(filteredExperiments, exps...)
	}

	return filteredExperiments, nil
}

func (svc *experimentService) validateExperimentOrthogonalityInDuration(
	experimentId *int64,
	settings models.Settings,
	segment models.ExperimentSegmentRaw,
	tier models.ExperimentTier,
	startTime time.Time,
	endTime time.Time,
) error {
	status := models.ExperimentStatusActive
	listExpParams := ListExperimentsParams{StartTime: &startTime, EndTime: &endTime, Status: &status, Tier: &tier}
	exps, err := svc.ListAllExperiments(settings.ProjectID, listExpParams)
	if err != nil {
		return err
	}
	return svc.validateExperimentOrthogonality(
		int64(settings.ProjectID),
		experimentId,
		segment,
		exps,
		settings.Config.Segmenters.Names,
	)
}

func (svc *experimentService) ValidatePairwiseExperimentOrthogonality(
	projectId int64,
	experiments []*models.Experiment,
	segmenters []string,
) error {
	segmenterTypes, err := svc.services.SegmenterService.GetSegmenterTypes(projectId)
	if err != nil {
		return err
	}

	// len(exps)-1 is used because the last element does not need to be checked. Inside the loop,
	// we do otherExps := exps[i+1:] and there are no elements afterwards beyond i==len(exps)-1
	for i := 0; i < len(experiments)-1; i++ {
		currExp := experiments[i]
		otherExps := experiments[i+1:] // Take all the remaining elements
		experimentId := currExp.ID.ToApiSchema()

		// Filter other experiments by the same tier
		otherExpsByTier := []*models.Experiment{}
		for _, item := range otherExps {
			if item.Tier == currExp.Tier {
				otherExpsByTier = append(otherExpsByTier, item)
			}
		}

		rawSegments, err := currExp.Segment.ToRawSchema(segmenterTypes)
		if err != nil {
			return err
		}
		err = svc.validateExperimentOrthogonality(
			projectId,
			&experimentId,
			rawSegments,
			otherExpsByTier,
			segmenters,
		)
		if err != nil {
			return errors.Newf(
				errors.BadInput,
				fmt.Sprintf("Orthogonality check for experiment ID %d: %s", currExp.ID, err.Error()),
			)
		}
	}

	return nil
}

// ValidateProjectExperimentSegmentersExist checks if the set of segmenters given contains all the segments specified
// by all the experiments
func (svc *experimentService) ValidateProjectExperimentSegmentersExist(
	projectId int64,
	experiments []*models.Experiment,
	segmenters []string,
) error {
	segmenterTypes, err := svc.services.SegmenterService.GetSegmenterTypes(projectId)
	if err != nil {
		return err
	}

	for _, exp := range experiments {
		rawSegments, err := exp.Segment.ToRawSchema(segmenterTypes)
		if err != nil {
			return err
		}
		// Check if the set of segmenters contains all the segments specified by the experiment
		err = validateExperimentSegmentersExist(
			exp.Name,
			rawSegments,
			utils.StringSliceToSet(segmenters),
		)
		if err != nil {
			return err
		}
	}

	return nil
}

// validateExperimentSegmentersExist checks if the set of segmenters contains all the segments given
func validateExperimentSegmentersExist(
	expName string,
	expSegment models.ExperimentSegmentRaw,
	segmenterNames *set.Set,
) error {
	if segmenterNames != nil {
		for segmentName := range expSegment {
			if !segmenterNames.Has(interface{}(segmentName)) {
				return fmt.Errorf("experiment %s requires segmenter: %s", expName, segmentName)
			}
		}
	}
	return nil
}

// RunCustomValidation validates the experiment by running all its treatments against the treatment schema AND itself
// against the validation/url given in the settings concurrently; if either of them return an error, this method
// returns an error
func (svc *experimentService) RunCustomValidation(
	experiment models.Experiment,
	settings models.Settings,
	context ValidationContext,
	operationType OperationType,
) error {
	g := new(errgroup.Group)

	for _, treatment := range experiment.Treatments {
		treatment := treatment
		g.Go(func() error {
			return ValidateTreatmentConfigWithTreatmentSchema(
				treatment.Configuration,
				settings.TreatmentSchema,
			)
		})
	}

	g.Go(func() error {
		return svc.services.ValidationService.ValidateEntityWithExternalUrl(operationType, EntityTypeExperiment,
			experiment,
			context,
			settings.ValidationUrl,
		)
	})

	if err := g.Wait(); err != nil {
		return err
	}
	return nil
}
