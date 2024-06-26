package pipelines

import (
	"errors"
	"fmt"
	"strings"

	// according to https://freshman.tech/snippets/go/check-if-slice-contains-element
	"golang.org/x/exp/slices"

	ort "github.com/yalue/onnxruntime_go"

	util "github.com/knights-analytics/hugot/utils"

	jsoniter "github.com/json-iterator/go"
	"github.com/knights-analytics/tokenizers"
)

// types

type TokenClassificationPipeline struct {
	BasePipeline
	IdLabelMap          map[int]string
	AggregationStrategy string
	IgnoreLabels        []string
}

type TokenClassificationPipelineConfig struct {
	IdLabelMap map[int]string `json:"id2label"`
}

type Entity struct {
	Entity    string
	Score     float32
	Scores    []float32
	Index     int
	Word      string
	TokenId   uint32
	Start     uint
	End       uint
	IsSubword bool
}

type TokenClassificationOutput struct {
	Entities [][]Entity
}

func (t *TokenClassificationOutput) GetOutput() []any {
	out := make([]any, len(t.Entities))
	for i, entity := range t.Entities {
		out[i] = any(entity)
	}
	return out
}

// options

func WithSimpleAggregation() PipelineOption[*TokenClassificationPipeline] {
	return func(pipeline *TokenClassificationPipeline) {
		pipeline.AggregationStrategy = "SIMPLE"
	}
}

func WithoutAggregation() PipelineOption[*TokenClassificationPipeline] {
	return func(pipeline *TokenClassificationPipeline) {
		pipeline.AggregationStrategy = "NONE"
	}
}

func WithIgnoreLabels(ignoreLabels []string) PipelineOption[*TokenClassificationPipeline] {
	return func(pipeline *TokenClassificationPipeline) {
		pipeline.IgnoreLabels = ignoreLabels
	}
}

// NewTokenClassificationPipeline Initializes a feature extraction pipeline
func NewTokenClassificationPipeline(config PipelineConfig[*TokenClassificationPipeline], ortOptions *ort.SessionOptions) (*TokenClassificationPipeline, error) {
	pipeline := &TokenClassificationPipeline{}
	pipeline.ModelPath = config.ModelPath
	pipeline.PipelineName = config.Name
	pipeline.OrtOptions = ortOptions
	pipeline.OnnxFilename = config.OnnxFilename
	for _, o := range config.Options {
		o(pipeline)
	}

	// inputs and encoding options
	pipeline.TokenizerOptions = []tokenizers.EncodeOption{
		tokenizers.WithReturnTokens(),
		tokenizers.WithReturnTypeIDs(),
		tokenizers.WithReturnAttentionMask(),
		tokenizers.WithReturnSpecialTokensMask(),
		tokenizers.WithReturnOffsets(),
	}

	// load json model config and set pipeline settings
	configPath := util.PathJoinSafe(config.ModelPath, "config.json")
	pipelineInputConfig := TokenClassificationPipelineConfig{}
	mapBytes, err := util.ReadFileBytes(configPath)
	if err != nil {
		return nil, err
	}

	err = jsoniter.Unmarshal(mapBytes, &pipelineInputConfig)
	if err != nil {
		return nil, err
	}
	pipeline.IdLabelMap = pipelineInputConfig.IdLabelMap

	pipeline.PipelineTimings = &Timings{}
	pipeline.TokenizerTimings = &Timings{}

	// defaults

	if pipeline.AggregationStrategy == "" {
		pipeline.AggregationStrategy = "SIMPLE"
	}
	if len(pipeline.IgnoreLabels) == 0 {
		pipeline.IgnoreLabels = []string{"O"}
	}

	// load onnx model
	errModel := pipeline.loadModel()
	if errModel != nil {
		return nil, errModel
	}

	// the dimension of the output is taken from the output meta.
	pipeline.OutputDim = int(pipeline.OutputsMeta[0].Dimensions[2])

	err = pipeline.Validate()
	if err != nil {
		return nil, err
	}
	return pipeline, nil
}

func (p *TokenClassificationPipeline) Validate() error {
	var validationErrors []error

	if p.OutputDim <= 0 {
		validationErrors = append(validationErrors, fmt.Errorf("p configuration invalid: outputDim parameter must be greater than zero"))
	}
	if len(p.IdLabelMap) <= 0 {
		validationErrors = append(validationErrors, fmt.Errorf("p configuration invalid: length of id2label map for token classification p must be greater than zero"))
	}
	if len(p.IdLabelMap) != p.OutputDim {
		validationErrors = append(validationErrors, fmt.Errorf("p configuration invalid: length of id2label map does not match model output dimension"))
	}
	return errors.Join(validationErrors...)
}

// Postprocess function for a token classification pipeline
func (p *TokenClassificationPipeline) Postprocess(batch PipelineBatch) (*TokenClassificationOutput, error) {

	outputs := make([][][]float32, len(batch.Input))        // holds the final output
	inputVectors := make([][]float32, 0, batch.MaxSequence) // holds the embeddings of each original token (no padding) for an input
	tokenVector := make([]float32, p.OutputDim)             // holds the vector embedding for a token
	inputTokens := batch.Input[0].TokenIds
	tokenVectorCounter := 0
	tokenCounter := 0
	inputCounter := 0
	nInputs := len(batch.Input)

	// construct the output vectors, however discard the embeddings of the padding tokens so that the output vector length
	// for an input is equal to the number of original tokens

	for _, result := range batch.OutputTensor {
		tokenVector[tokenVectorCounter] = result
		if tokenVectorCounter == p.OutputDim-1 {
			// raw result vector for token is now complete
			if tokenCounter < len(inputTokens) {
				// it is an original token (not resulting from padding), keep it
				inputVectors = append(inputVectors, util.SoftMax(tokenVector))
			}
			tokenVectorCounter = 0
			tokenVector = make([]float32, p.OutputDim)
			if tokenCounter == batch.MaxSequence-1 {
				// we went through all tokens in the sequence for this input
				outputs[inputCounter] = inputVectors
				tokenCounter = 0
				inputVectors = make([][]float32, 0, batch.MaxSequence)
				inputCounter++
				if inputCounter < nInputs {
					inputTokens = batch.Input[inputCounter].TokenIds
				}
			} else {
				tokenCounter++
			}
		} else {
			tokenVectorCounter++
		}
	}

	// now convert the logits to the predictions of actual entities
	classificationOutput := TokenClassificationOutput{
		Entities: make([][]Entity, len(batch.Input)),
	}

	for i, input := range batch.Input {
		preEntities := p.GatherPreEntities(input, outputs[i])
		entities, errAggregate := p.Aggregate(input, preEntities)
		if errAggregate != nil {
			return nil, errAggregate
		}
		// Filter anything that is in ignore_labels
		var filteredEntities []Entity
		for _, e := range entities {
			if !slices.Contains(p.IgnoreLabels, e.Entity) && e.Entity != "" {
				filteredEntities = append(filteredEntities, e)
			}
		}
		classificationOutput.Entities[i] = filteredEntities
	}
	return &classificationOutput, nil
}

// GatherPreEntities from batch of logits to list of pre-aggregated outputs
func (p *TokenClassificationPipeline) GatherPreEntities(input TokenizedInput, output [][]float32) []Entity {

	sentence := input.Raw
	var preEntities []Entity

	for j, tokenScores := range output {

		// filter out special tokens (skip them)
		if input.SpecialTokensMask[j] > 0.0 {
			continue
		}
		// TODO: the python code uses id_to_token to get the token here which is a method on the rust tokenizer, check if it's better
		word := input.Tokens[j]
		tokenId := input.TokenIds[j]
		// TODO: the determination of subword can probably be better done by exporting the words field from the tokenizer directly
		startInd := input.Offsets[j][0]
		endInd := input.Offsets[j][1]
		wordRef := sentence[startInd:endInd]
		isSubword := len(word) != len(wordRef)
		// TODO: check for unknown token here, it's in the config and can be loaded and compared with the token
		// in that case set the subword as in the python code
		preEntities = append(preEntities, Entity{
			Word:      word,
			TokenId:   tokenId,
			Scores:    tokenScores,
			Start:     startInd,
			End:       endInd,
			Index:     j,
			IsSubword: isSubword,
		})
	}
	return preEntities
}

func (p *TokenClassificationPipeline) Aggregate(input TokenizedInput, preEntities []Entity) ([]Entity, error) {
	entities := make([]Entity, len(preEntities))
	if p.AggregationStrategy == "SIMPLE" || p.AggregationStrategy == "NONE" {
		for i, preEntity := range preEntities {
			entityIdx, score, argMaxErr := util.ArgMax(preEntity.Scores)
			if argMaxErr != nil {
				return nil, argMaxErr
			}
			label, ok := p.IdLabelMap[entityIdx]
			if !ok {
				return nil, fmt.Errorf("could not determine entity type for input %s, predicted entity index %d", input.Raw, entityIdx)
			}
			entities[i] = Entity{
				Entity:  label,
				Score:   score,
				Index:   preEntity.Index,
				Word:    preEntity.Word,
				TokenId: preEntity.TokenId,
				Start:   preEntity.Start,
				End:     preEntity.End,
			}
		}
	} else {
		return nil, errors.New("aggregation strategies other than SIMPLE and NONE are not implemented")
	}
	if p.AggregationStrategy == "NONE" {
		return entities, nil
	}
	return p.GroupEntities(entities)
}

func (p *TokenClassificationPipeline) getTag(entityName string) (string, string) {
	var bi string
	var tag string
	if strings.HasPrefix(entityName, "B-") {
		bi = "B"
		tag = entityName[2:]
	} else if strings.HasPrefix(entityName, "I-") {
		bi = "I"
		tag = entityName[2:]
	} else {
		// defaulting to "I" if string is not in B- I- format
		bi = "I"
		tag = entityName
	}
	return bi, tag
}

func (p *TokenClassificationPipeline) groupSubEntities(entities []Entity) Entity {
	splits := strings.Split(entities[0].Entity, "-")
	var entityType string
	if len(splits) == 1 {
		entityType = splits[0]
	} else {
		entityType = strings.Join(splits[1:], "-")
	}
	scores := make([]float32, len(entities))
	tokens := make([]uint32, len(entities))
	for i, s := range entities {
		scores[i] = s.Score
		tokens[i] = s.TokenId
	}
	score := util.Mean(scores)
	// note: here we directly appeal to the tokenizer decoder with the tokenIds
	// in the python code they pass the words to a token_to_string_method
	word := p.Tokenizer.Decode(tokens, false)

	return Entity{
		Entity: entityType,
		Score:  score,
		Word:   word,
		Start:  entities[0].Start,
		End:    entities[len(entities)-1].End,
	}
}

// GroupEntities group together adjacent tokens with the same entity predicted
func (p *TokenClassificationPipeline) GroupEntities(entities []Entity) ([]Entity, error) {
	var entityGroups []Entity
	var currentGroupDisagg []Entity

	for _, e := range entities {
		if len(currentGroupDisagg) == 0 {
			currentGroupDisagg = append(currentGroupDisagg, e)
			continue
		}

		bi, tag := p.getTag(e.Entity)
		_, lastTag := p.getTag(currentGroupDisagg[len(currentGroupDisagg)-1].Entity)
		if tag == lastTag && bi != "B" {
			currentGroupDisagg = append(currentGroupDisagg, e)
		} else {
			// create the grouped entity
			entityGroups = append(entityGroups, p.groupSubEntities(currentGroupDisagg))
			currentGroupDisagg = []Entity{e}
		}
	}

	if len(currentGroupDisagg) > 0 {
		// last entity remaining
		entityGroups = append(entityGroups, p.groupSubEntities(currentGroupDisagg))
	}
	return entityGroups, nil
}

// Run the pipeline on a string batch
func (p *TokenClassificationPipeline) Run(inputs []string) (PipelineBatchOutput, error) {
	return p.RunPipeline(inputs)
}

func (p *TokenClassificationPipeline) RunPipeline(inputs []string) (*TokenClassificationOutput, error) {
	batch := p.Preprocess(inputs)
	batch, errForward := p.Forward(batch)
	if errForward != nil {
		return nil, errForward
	}
	return p.Postprocess(batch)
}
