package pipelines

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/daulet/tokenizers"
	ort "github.com/yalue/onnxruntime_go"

	util "github.com/knights-analytics/hugot/utils"

	jsoniter "github.com/json-iterator/go"
)

// TokenClassificationPipeline is a go version of huggingface tokenClassificationPipeline.
// https://github.com/huggingface/transformers/blob/main/src/transformers/pipelines/token_classification.py
type TokenClassificationPipeline struct {
	basePipeline
	IDLabelMap          map[int]string
	AggregationStrategy string
	IgnoreLabels        []string
}

type TokenClassificationPipelineConfig struct {
	IDLabelMap map[int]string `json:"id2label"`
}

type Entity struct {
	Entity    string
	Score     float32
	Scores    []float32
	Index     int
	Word      string
	TokenID   uint32
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

// TODO: need to implement the other types of aggregation (max etc)

// WithSimpleAggregation sets the aggregation strategy for the token labels to simple
// It reproduces simple aggregation from the huggingface implementation.
func WithSimpleAggregation() PipelineOption[*TokenClassificationPipeline] {
	return func(pipeline *TokenClassificationPipeline) {
		pipeline.AggregationStrategy = "SIMPLE"
	}
}

// WithoutAggregation returns the token labels.
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

// NewTokenClassificationPipeline Initializes a feature extraction pipeline.
func NewTokenClassificationPipeline(config PipelineConfig[*TokenClassificationPipeline], ortOptions *ort.SessionOptions) (*TokenClassificationPipeline, error) {
	pipeline := &TokenClassificationPipeline{}
	pipeline.ModelPath = config.ModelPath
	pipeline.PipelineName = config.Name
	pipeline.OrtOptions = ortOptions
	pipeline.OnnxFilename = config.OnnxFilename
	for _, o := range config.Options {
		o(pipeline)
	}

	// onnx model init
	model, err := loadOnnxModelBytes(pipeline.ModelPath, pipeline.OnnxFilename)
	if err != nil {
		return nil, err
	}

	// init of inputs and outputs
	inputs, outputs, err := loadInputOutputMeta(model)
	if err != nil {
		return nil, err
	}
	pipeline.InputsMeta = inputs
	pipeline.OutputsMeta = outputs

	// Id label map
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
	pipeline.IDLabelMap = pipelineInputConfig.IDLabelMap

	// default strategies if not set
	if pipeline.AggregationStrategy == "" {
		pipeline.AggregationStrategy = "SIMPLE"
	}
	if len(pipeline.IgnoreLabels) == 0 {
		pipeline.IgnoreLabels = []string{"O"}
	}

	pipeline.PipelineTimings = &timings{}
	pipeline.TokenizerTimings = &timings{}

	// tokenizer init
	pipeline.TokenizerOptions, err = getTokenizerOptions(inputs)
	if err != nil {
		return nil, err
	}
	// Additional options needed for postprocessing
	pipeline.TokenizerOptions = append(pipeline.TokenizerOptions,
		tokenizers.WithReturnSpecialTokensMask(),
		tokenizers.WithReturnOffsets(),
	)
	tk, tkErr := loadTokenizer(pipeline.ModelPath)
	if tkErr != nil {
		return nil, tkErr
	}
	pipeline.Tokenizer = tk

	// creation of the session. Only one output (either token or sentence embedding).
	session, err := createSession(model, inputs, outputs, ortOptions)
	if err != nil {
		return nil, err
	}
	pipeline.OrtSession = session

	err = pipeline.Validate()
	if err != nil {
		return nil, err
	}
	return pipeline, nil
}

// INTERFACE IMPLEMENTATION

// GetMetadata returns metadata information about the pipeline, in particular:
// OutputInfo: names and dimensions of the output layer used for token classification.
func (p *TokenClassificationPipeline) GetMetadata() PipelineMetadata {
	return PipelineMetadata{
		OutputsInfo: []OutputInfo{
			{
				Name:       p.OutputsMeta[0].Name,
				Dimensions: p.OutputsMeta[0].Dimensions,
			},
		},
	}
}

// Destroy frees the feature extraction pipeline resources.
func (p *TokenClassificationPipeline) Destroy() error {
	return destroySession(p.Tokenizer, p.OrtSession)
}

// GetStats returns the runtime statistics for the pipeline.
func (p *TokenClassificationPipeline) GetStats() []string {
	return []string{
		fmt.Sprintf("Statistics for pipeline: %s", p.PipelineName),
		fmt.Sprintf("Tokenizer: Total time=%s, Execution count=%d, Average query time=%s",
			time.Duration(p.TokenizerTimings.TotalNS),
			p.TokenizerTimings.NumCalls,
			time.Duration(float64(p.TokenizerTimings.TotalNS)/math.Max(1, float64(p.TokenizerTimings.NumCalls)))),
		fmt.Sprintf("ONNX: Total time=%s, Execution count=%d, Average query time=%s",
			time.Duration(p.PipelineTimings.TotalNS),
			p.PipelineTimings.NumCalls,
			time.Duration(float64(p.PipelineTimings.TotalNS)/math.Max(1, float64(p.PipelineTimings.NumCalls)))),
	}
}

// Validate checks that the pipeline is valid.
func (p *TokenClassificationPipeline) Validate() error {
	var validationErrors []error

	outputDim := p.OutputsMeta[0].Dimensions
	if len(outputDim) != 3 {
		validationErrors = append(validationErrors,
			fmt.Errorf("output for token classification must be three dimensional (input, sequence, logits)"))
	}

	if outputDim[len(outputDim)-1] == -1 {
		validationErrors = append(validationErrors,
			fmt.Errorf("logit dimension cannot be dynamic"))
	}
	if len(p.IDLabelMap) <= 0 {
		validationErrors = append(validationErrors, fmt.Errorf("p configuration invalid: length of id2label map for token classification p must be greater than zero"))
	}
	return errors.Join(validationErrors...)
}

// Preprocess tokenizes the input strings.
func (p *TokenClassificationPipeline) Preprocess(batch *PipelineBatch, inputs []string) error {
	start := time.Now()
	tokenizeInputs(batch, p.Tokenizer, inputs, p.TokenizerOptions)
	atomic.AddUint64(&p.TokenizerTimings.NumCalls, 1)
	atomic.AddUint64(&p.TokenizerTimings.TotalNS, uint64(time.Since(start)))
	err := createInputTensors(batch, p.InputsMeta)
	return err
}

// Forward performs the forward inference of the pipeline.
func (p *TokenClassificationPipeline) Forward(batch *PipelineBatch) error {
	start := time.Now()
	err := runSessionOnBatch(batch, p.OrtSession, p.OutputsMeta)
	if err != nil {
		return err
	}
	atomic.AddUint64(&p.PipelineTimings.NumCalls, 1)
	atomic.AddUint64(&p.PipelineTimings.TotalNS, uint64(time.Since(start)))
	return nil
}

// Postprocess function for a token classification pipeline.
func (p *TokenClassificationPipeline) Postprocess(batch *PipelineBatch) (*TokenClassificationOutput, error) {
	if len(batch.Input) == 0 {
		return &TokenClassificationOutput{}, nil
	}

	outputDims := p.OutputsMeta[0].Dimensions
	tokenLogitsDim := int(outputDims[len(outputDims)-1])
	outputs := make([][][]float32, len(batch.Input))              // holds the final output
	inputVectors := make([][]float32, 0, batch.MaxSequenceLength) // holds the embeddings of each original token (no padding) for an input
	tokenVector := make([]float32, tokenLogitsDim)                // holds the vector embedding for a token
	inputTokens := batch.Input[0].TokenIDs                        // original tokens from the input excluding the padded tokens
	tokenVectorCounter := 0
	tokenCounter := 0
	inputCounter := 0
	nInputs := len(batch.Input)

	// construct the output vectors by gathering the logits,
	// however discard the embeddings of the padding tokens so that the output vector length
	// for an input is equal to the number of original tokens
	outputTensor := batch.OutputValues[0].(*ort.Tensor[float32])
	for _, result := range outputTensor.GetData() {
		tokenVector[tokenVectorCounter] = result
		if tokenVectorCounter == tokenLogitsDim-1 {
			// raw result vector for token is now complete
			if tokenCounter < len(inputTokens) {
				// it is an original token (not resulting from padding), keep it
				inputVectors = append(inputVectors, util.SoftMax(tokenVector))
			}
			tokenVectorCounter = 0
			tokenVector = make([]float32, tokenLogitsDim)
			if tokenCounter == batch.MaxSequenceLength-1 {
				// we went through all tokens in the sequence for this input
				outputs[inputCounter] = inputVectors
				tokenCounter = 0
				inputVectors = make([][]float32, 0, batch.MaxSequenceLength)
				inputCounter++
				if inputCounter < nInputs {
					inputTokens = batch.Input[inputCounter].TokenIDs
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
func (p *TokenClassificationPipeline) GatherPreEntities(input tokenizedInput, output [][]float32) []Entity {
	sentence := input.Raw
	var preEntities []Entity

	for j, tokenScores := range output {

		// filter out special tokens (skip them)
		if input.SpecialTokensMask[j] > 0.0 {
			continue
		}
		// TODO: the python code uses id_to_token to get the token here which is a method on the rust tokenizer, check if it's better
		word := input.Tokens[j]
		tokenID := input.TokenIDs[j]
		// TODO: the determination of subword can probably be better done by exporting the words field from the tokenizer directly
		startInd := input.Offsets[j][0]
		endInd := input.Offsets[j][1]
		wordRef := sentence[startInd:endInd]
		isSubword := len(word) != len(wordRef)
		// TODO: check for unknown token here, it's in the config and can be loaded and compared with the token
		// in that case set the subword as in the python code
		preEntities = append(preEntities, Entity{
			Word:      word,
			TokenID:   tokenID,
			Scores:    tokenScores,
			Start:     startInd,
			End:       endInd,
			Index:     j,
			IsSubword: isSubword,
		})
	}
	return preEntities
}

func (p *TokenClassificationPipeline) Aggregate(input tokenizedInput, preEntities []Entity) ([]Entity, error) {
	entities := make([]Entity, len(preEntities))
	if p.AggregationStrategy == "SIMPLE" || p.AggregationStrategy == "NONE" {
		for i, preEntity := range preEntities {
			entityIdx, score, argMaxErr := util.ArgMax(preEntity.Scores)
			if argMaxErr != nil {
				return nil, argMaxErr
			}
			label, ok := p.IDLabelMap[entityIdx]
			if !ok {
				return nil, fmt.Errorf("could not determine entity type for input %s, predicted entity index %d", input.Raw, entityIdx)
			}
			entities[i] = Entity{
				Entity:  label,
				Score:   score,
				Index:   preEntity.Index,
				Word:    preEntity.Word,
				TokenID: preEntity.TokenID,
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
		tokens[i] = s.TokenID
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

// GroupEntities group together adjacent tokens with the same entity predicted.
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

// Run the pipeline on a string batch.
func (p *TokenClassificationPipeline) Run(inputs []string) (PipelineBatchOutput, error) {
	return p.RunPipeline(inputs)
}

// RunPipeline is like Run but returns the concrete type rather than the interface.
func (p *TokenClassificationPipeline) RunPipeline(inputs []string) (*TokenClassificationOutput, error) {
	var runErrors []error
	batch := NewBatch()
	defer func(*PipelineBatch) {
		runErrors = append(runErrors, batch.Destroy())
	}(batch)

	runErrors = append(runErrors, p.Preprocess(batch, inputs))
	if e := errors.Join(runErrors...); e != nil {
		return nil, e
	}

	runErrors = append(runErrors, p.Forward(batch))
	if e := errors.Join(runErrors...); e != nil {
		return nil, e
	}

	result, postErr := p.Postprocess(batch)
	runErrors = append(runErrors, postErr)
	return result, errors.Join(runErrors...)
}
