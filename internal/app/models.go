package app

import (
	"time"

	"jarlore/internal/domain"
)

// Read models: the JSON shapes of docs/api/openapi.yaml. A contract test
// (internal/api/contract_test.go) validates real responses against the spec.

type RecipeVersionRef struct {
	RecipeID   string `json:"recipeId"`
	RecipeName string `json:"recipeName"`
	VersionID  string `json:"versionId"`
	VersionNo  int    `json:"versionNo"`
	Scope      string `json:"scope"`
}

type LogEntry struct {
	ID           string               `json:"id"`
	BatchID      string               `json:"batchId"`
	BottleID     *string              `json:"bottleId"`
	Kind         string               `json:"kind"`
	OccurredAt   time.Time            `json:"occurredAt"`
	Note         *string              `json:"note"`
	Measurements []domain.Measurement `json:"measurements"`
	CreatedAt    time.Time            `json:"createdAt"`
}

type Reminder struct {
	ID       string    `json:"id"`
	BatchID  string    `json:"batchId"`
	BottleID *string   `json:"bottleId"`
	Kind     string    `json:"kind"`
	DueAt    time.Time `json:"dueAt"`
	Status   string    `json:"status"`
	Label    string    `json:"label"`
}

type Bottle struct {
	ID               string               `json:"id"`
	BatchID          string               `json:"batchId"`
	Label            string               `json:"label"`
	FlavorName       string               `json:"flavorName"`
	VolumeMl         int                  `json:"volumeMl"`
	Additions        []domain.Addition    `json:"additions"`
	PrimingSugarG    *float64             `json:"primingSugarG"`
	HeadspaceCm      *float64             `json:"headspaceCm"`
	Status           string               `json:"status"`
	StartedAt        time.Time            `json:"startedAt"`
	FinishedAt       *time.Time           `json:"finishedAt"`
	LastMeasurements []domain.Measurement `json:"lastMeasurements"`
	Rating           *domain.Rating       `json:"rating"`
	RecipeVersion    *RecipeVersionRef    `json:"recipeVersion"`
	ParentBottleID   *string              `json:"parentBottleId"`
	Hypothesis       *string              `json:"hypothesis"`
	ChangedFields    []string             `json:"changedFields"`
	IsReference      bool                 `json:"isReference"`
}

type batchCore struct {
	ID                 string            `json:"id"`
	Code               string            `json:"code"`
	Name               string            `json:"name"`
	FermentationTypeID string            `json:"fermentationTypeId"`
	Status             string            `json:"status"`
	Stage              string            `json:"stage"`
	ArchivedAt         *time.Time        `json:"archivedAt"`
	StartedAt          time.Time         `json:"startedAt"`
	FinishedAt         *time.Time        `json:"finishedAt"`
	VolumeMl           int               `json:"volumeMl"`
	Vessel             *string           `json:"vessel"`
	RecipeVersion      *RecipeVersionRef `json:"recipeVersion"`
	ParentBatchID      *string           `json:"parentBatchId"`
	Hypothesis         *string           `json:"hypothesis"`
	ChangedFields      []string          `json:"changedFields"`
	IsReference        bool              `json:"isReference"`
	Rating             *domain.Rating    `json:"rating"`
	LastLog            *LogEntry         `json:"lastLog"`
	NextReminder       *Reminder         `json:"nextReminder"`
}

type BatchSummary struct {
	batchCore
	BottleCount int `json:"bottleCount"`
}

type Batch struct {
	batchCore
	Ingredients []domain.Ingredient `json:"ingredients"`
	Params      domain.BatchParams  `json:"params"`
	Bottles     []Bottle            `json:"bottles"`
}

type Page[T any] struct {
	Data       []T     `json:"data"`
	NextCursor *string `json:"nextCursor"`
}

type RecipeVersion struct {
	ID              string                 `json:"id"`
	RecipeID        string                 `json:"recipeId"`
	VersionNo       int                    `json:"versionNo"`
	ParentVersionID *string                `json:"parentVersionId"`
	ChangeNote      *string                `json:"changeNote"`
	SourceBatchID   *string                `json:"sourceBatchId"`
	SourceBottleID  *string                `json:"sourceBottleId"`
	BaseVolumeMl    int                    `json:"baseVolumeMl"`
	Ingredients     []domain.Ingredient    `json:"ingredients"`
	Params          domain.BatchParams     `json:"params"`
	DefaultBottles  []domain.DefaultBottle `json:"defaultBottles"`
	Bottle          *domain.BottleSpec     `json:"bottle"`
	ChangedFields   []string               `json:"changedFields"`
	CreatedAt       time.Time              `json:"createdAt"`
	BatchCount      int                    `json:"batchCount"`
	AvgOverall      *float64               `json:"avgOverall"`
}

// RecipeVersionDetail is a version read on its own (GET /v1/recipe-versions/{id}).
type RecipeVersionDetail struct {
	RecipeVersion
	RecipeName         string `json:"recipeName"`
	Scope              string `json:"scope"`
	FermentationTypeID string `json:"fermentationTypeId"`
}

type Recipe struct {
	ID                 string        `json:"id"`
	Name               string        `json:"name"`
	Scope              string        `json:"scope"`
	FermentationTypeID string        `json:"fermentationTypeId"`
	LatestVersion      RecipeVersion `json:"latestVersion"`
	VersionCount       int           `json:"versionCount"`
	BatchCount         int           `json:"batchCount"`
	AvgOverall         *float64      `json:"avgOverall"`
	ArchivedAt         *time.Time    `json:"archivedAt"`
	CreatedAt          time.Time     `json:"createdAt"`
}

type DiffResult struct {
	Rows        []domain.Row `json:"rows"`
	ChangeCount int          `json:"changeCount"`
	// Brew Again previews only: whether starting this draft creates a new recipe version.
	WillCreateVersion *bool `json:"willCreateVersion,omitempty"`
	NextVersionNo     *int  `json:"nextVersionNo,omitempty"`
}

type ItemRef struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type CompareItem struct {
	Ref         ItemRef `json:"ref"`
	Title       string  `json:"title"`
	Subtitle    *string `json:"subtitle"`
	IsReference bool    `json:"isReference"`
}

type CompareResult struct {
	Items []CompareItem `json:"items"`
	Rows  []domain.Row  `json:"rows"`
}

type FermentationStage struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	TypicalDays [2]int `json:"typicalDays"`
}

type FermentationType struct {
	ID                 string                   `json:"id"`
	Name               string                   `json:"name"`
	Stages             []FermentationStage      `json:"stages"`
	DefaultVolumeMl    int                      `json:"defaultVolumeMl"`
	DefaultIngredients []domain.IngredientInput `json:"defaultIngredients"`
	DefaultParams      domain.BatchParams       `json:"defaultParams"`
}

type MeasurementDefinition struct {
	ID             string   `json:"id"`
	Key            string   `json:"key"`
	Label          string   `json:"label"`
	Kind           string   `json:"kind"`
	Unit           string   `json:"unit"`
	Decimals       int      `json:"decimals"`
	Min            *float64 `json:"min"`
	Max            *float64 `json:"max"`
	RequiresMethod bool     `json:"requiresMethod"`
	IsSystem       bool     `json:"isSystem"`
}

type Me struct {
	ID          string    `json:"id"`
	Email       *string   `json:"email"`
	DisplayName *string   `json:"displayName"`
	UnitSystem  string    `json:"unitSystem"`
	TempUnit    string    `json:"tempUnit"`
	Timezone    string    `json:"timezone"`
	CreatedAt   time.Time `json:"createdAt"`
}

type Entitlements struct {
	Plan   string `json:"plan"`
	Limits struct {
		CompareItems       int  `json:"compareItems"`
		CustomMeasurements *int `json:"customMeasurements"`
	} `json:"limits"`
	Source string `json:"source"`
}

type Today struct {
	Active           []BatchSummary `json:"active"`
	Due              []Reminder     `json:"due"`
	RecentlyFinished []BatchSummary `json:"recentlyFinished"`
}

type BrewAgainDraft struct {
	Draft         CreateBatchInput  `json:"draft"`
	Base          BrewAgainBase     `json:"base"`
	RecipeVersion *RecipeVersionRef `json:"recipeVersion"`
}

type BrewAgainBase struct {
	Ref   ItemRef `json:"ref"`
	Title string  `json:"title"`
}

type BatchSnapshot struct {
	BatchID        string                 `json:"batchId"`
	CapturedAt     time.Time              `json:"capturedAt"`
	RecipeVersion  RecipeVersionRef       `json:"recipeVersion"`
	Ingredients    []domain.Ingredient    `json:"ingredients"`
	Params         domain.BatchParams     `json:"params"`
	DefaultBottles []domain.DefaultBottle `json:"defaultBottles"`
	Deviations     []domain.Row           `json:"deviations"`
}

type Reference struct {
	Batch  *BatchSummary `json:"batch"`
	Reason *string       `json:"reason"`
}
