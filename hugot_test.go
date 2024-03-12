package hugot_test

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path"
	"strings"
	"testing"

	"github.com/knights-analytics/hugot"
	"github.com/knights-analytics/hugot/pipelines"
	util "github.com/knights-analytics/hugot/utils"
	"github.com/stretchr/testify/assert"
)

//go:embed testData/tokenExpected.json
var tokenExpectedByte []byte

//go:embed testData/vectors.json
var resultsByte []byte

// use the system library for the tests
var onnxruntimeSharedLibrary = "/usr/lib64/onnxruntime.so"

// Text classification

func TestTextClassificationPipeline(t *testing.T) {
	session, err := hugot.NewSession(hugot.WithOnnxLibraryPath(onnxruntimeSharedLibrary))
	check(t, err)
	defer func(session *hugot.Session) {
		err := session.Destroy()
		check(t, err)
	}(session)
	modelFolder := os.Getenv("TEST_MODELS_FOLDER")
	if modelFolder == "" {
		modelFolder = "./models"
	}
	modelPath := path.Join(modelFolder, "distilbert-base-uncased-finetuned-sst-2-english")
	sentimentPipeline, err := session.NewTextClassificationPipeline(modelPath, "testPipeline")
	check(t, err)

	tests := []struct {
		pipeline *pipelines.TextClassificationPipeline
		name     string
		strings  []string
		expected pipelines.TextClassificationOutput
	}{
		{
			pipeline: sentimentPipeline,
			name:     "Basic tests",
			strings:  []string{"This movie is disgustingly good!", "The director tried too much"},
			expected: pipelines.TextClassificationOutput{
				ClassificationOutputs: [][]pipelines.ClassificationOutput{
					{
						{
							Label: "POSITIVE",
							Score: 0.9998536109924316,
						},
					},
					{
						{
							Label: "NEGATIVE",
							Score: 0.9975218176841736,
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			batchResult, err := tt.pipeline.Run(tt.strings)
			result, ok := batchResult.(*pipelines.TextClassificationOutput)
			if !ok {
				t.FailNow()
			}
			check(t, err)
			for i, expected := range tt.expected.ClassificationOutputs {
				checkClassificationOutput(t, expected, result.ClassificationOutputs[i])
			}
		})
	}

	// check get stats
	session.GetStats()
}

func TestNewSessionErrors(t *testing.T) {
	_, err := hugot.NewSession(hugot.WithOnnxLibraryPath(""))
	assert.Error(t, err)
}

func TestTextClassificationPipelineValidation(t *testing.T) {
	session, err := hugot.NewSession(hugot.WithOnnxLibraryPath(onnxruntimeSharedLibrary))
	check(t, err)
	defer func(session *hugot.Session) {
		err := session.Destroy()
		check(t, err)
	}(session)
	modelFolder := os.Getenv("TEST_MODELS_FOLDER")
	if modelFolder == "" {
		modelFolder = "./models"
	}
	modelPath := path.Join(modelFolder, "distilbert-base-uncased-finetuned-sst-2-english")
	sentimentPipeline, err := session.NewTextClassificationPipeline(modelPath, "testPipeline", pipelines.WithAggregationFunction(util.SoftMax))
	check(t, err)
	sentimentPipeline.IdLabelMap = map[int]string{}
	err = sentimentPipeline.Validate()
	assert.Error(t, err)
	if err != nil {
		errInt := err.(interface{ Unwrap() []error })
		assert.Equal(t, 3, len(errInt.Unwrap()))
	}
	sentimentPipeline.OutputDim = 0
	err = sentimentPipeline.Validate()
	assert.Error(t, err)
	if err != nil {
		errInt := err.(interface{ Unwrap() []error })
		assert.Equal(t, 3, len(errInt.Unwrap()))
	}
}

// Token classification

func TestTokenClassificationPipeline(t *testing.T) {
	session, err := hugot.NewSession(hugot.WithOnnxLibraryPath(onnxruntimeSharedLibrary))
	check(t, err)
	defer func(session *hugot.Session) {
		err := session.Destroy()
		check(t, err)
	}(session)

	modelFolder := os.Getenv("TEST_MODELS_FOLDER")
	if modelFolder == "" {
		modelFolder = "./models"
	}
	modelPath := path.Join(modelFolder, "distilbert-NER")
	pipelineSimple, err2 := session.NewTokenClassificationPipeline(modelPath, "testPipelineSimple", pipelines.WithSimpleAggregation(), pipelines.WithIgnoreLabels([]string{"O"}))
	check(t, err2)
	pipelineNone, err3 := session.NewTokenClassificationPipeline(modelPath, "testPipelineNone", pipelines.WithoutAggregation())
	check(t, err3)

	var expectedResults map[int]pipelines.TokenClassificationOutput
	err4 := json.Unmarshal(tokenExpectedByte, &expectedResults)
	check(t, err4)

	tests := []struct {
		pipeline *pipelines.TokenClassificationPipeline
		name     string
		strings  []string
		expected pipelines.TokenClassificationOutput
	}{
		{
			pipeline: pipelineSimple,
			name:     "Simple aggregation",
			strings:  []string{"My name is Wolfgang and I live in Berlin."},
			expected: expectedResults[0],
		},
		{
			pipeline: pipelineNone,
			name:     "No aggregation",
			strings:  []string{"My name is Wolfgang and I live in Berlin."},
			expected: expectedResults[1],
		},
		{
			pipeline: pipelineSimple,
			name:     "Parsing of batch with different token length",
			strings:  []string{"Microsoft incorporated.", "Yesterday I went to Berlin and met with Jack Brown."},
			expected: expectedResults[2],
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			batchResult, err := tt.pipeline.Run(tt.strings)
			check(t, err)
			result, ok := batchResult.(*pipelines.TokenClassificationOutput)
			if !ok {
				t.FailNow()
			}

			printTokenEntities(result)
			for i, predictedEntities := range result.Entities {
				assert.Equal(t, len(tt.expected.Entities[i]), len(predictedEntities))
				for j, entity := range predictedEntities {
					expectedEntity := tt.expected.Entities[i][j]
					assert.Equal(t, expectedEntity.Entity, entity.Entity)
					assert.Equal(t, expectedEntity.Word, entity.Word)
				}
			}
		})
	}
}

func TestTokenClassificationPipelineValidation(t *testing.T) {
	session, err := hugot.NewSession(hugot.WithOnnxLibraryPath(onnxruntimeSharedLibrary))
	check(t, err)
	defer func(session *hugot.Session) {
		err := session.Destroy()
		check(t, err)
	}(session)

	modelFolder := os.Getenv("TEST_MODELS_FOLDER")
	if modelFolder == "" {
		modelFolder = "./models"
	}
	modelPath := path.Join(modelFolder, "distilbert-NER")
	pipelineSimple, err2 := session.NewTokenClassificationPipeline(modelPath, "testPipelineSimple", pipelines.WithSimpleAggregation())
	check(t, err2)

	pipelineSimple.IdLabelMap = map[int]string{}
	err = pipelineSimple.Validate()
	assert.Error(t, err)
	if err != nil {
		errInt := err.(interface{ Unwrap() []error })
		assert.Equal(t, 2, len(errInt.Unwrap()))
	}
	pipelineSimple.OutputDim = 0
	err = pipelineSimple.Validate()
	assert.Error(t, err)
	if err != nil {
		errInt := err.(interface{ Unwrap() []error })
		assert.Equal(t, 2, len(errInt.Unwrap()))
	}
}

// feature extraction

func TestFeatureExtractionPipeline(t *testing.T) {
	session, err := hugot.NewSession(hugot.WithOnnxLibraryPath(onnxruntimeSharedLibrary))
	check(t, err)
	defer func(session *hugot.Session) {
		err := session.Destroy()
		check(t, err)
	}(session)

	modelFolder := os.Getenv("TEST_MODELS_FOLDER")
	if modelFolder == "" {
		modelFolder = "./models"
	}
	modelPath := path.Join(modelFolder, "all-MiniLM-L6-v2")
	pipeline, err := session.NewFeatureExtractionPipeline(modelPath, "testPipeline")
	check(t, err)

	var expectedResults map[string][][]float32
	err = json.Unmarshal(resultsByte, &expectedResults)
	check(t, err)
	var testResults [][]float32

	// test 'robert smith'
	testResults = expectedResults["test1output"]
	for i := 1; i <= 10; i++ {
		batchResult, err := pipeline.Run([]string{"robert smith"})
		check(t, err)
		result, ok := batchResult.(*pipelines.FeatureExtractionOutput)
		if !ok {
			t.FailNow()
		}
		e := floatsEqual(result.Embeddings[0], testResults[0])
		if e != nil {
			t.Logf("Test 1: The neural network didn't produce the correct result on loop %d: %s\n", i, e)
			t.FailNow()
		}
	}

	// test ['robert smith junior', 'francis ford coppola']
	testResults = expectedResults["test2output"]
	for i := 1; i <= 10; i++ {
		batchResult, err := pipeline.Run([]string{"robert smith junior", "francis ford coppola"})
		check(t, err)
		result, ok := batchResult.(*pipelines.FeatureExtractionOutput)
		if !ok {
			t.FailNow()
		}
		for j, res := range result.Embeddings {
			e := floatsEqual(res, testResults[j])
			if e != nil {
				t.Logf("Test 2: The neural network didn't produce the correct result on loop %d: %s\n", i, e)
				t.FailNow()
			}
		}
	}

	// determinism test to make sure embeddings of a string are not influenced by other strings in the batch
	testPairs := map[string][][]string{}
	testPairs["identity"] = [][]string{{"sinopharm", "yo"}, {"sinopharm", "yo"}}
	testPairs["contextOverlap"] = [][]string{{"sinopharm", "yo"}, {"sinopharm", "yo mama yo"}}
	testPairs["contextDisjoint"] = [][]string{{"sinopharm", "yo"}, {"sinopharm", "another test"}}

	for k, sentencePair := range testPairs {
		// these vectors should be the same
		firstBatchResult, err2 := pipeline.Run(sentencePair[0])
		check(t, err2)
		firstResult, ok := firstBatchResult.(*pipelines.FeatureExtractionOutput)
		if !ok {
			t.FailNow()
		}
		firstEmbedding := firstResult.Embeddings[0]

		secondBatchResult, err3 := pipeline.Run(sentencePair[1])
		check(t, err3)
		secondResult, ok := secondBatchResult.(*pipelines.FeatureExtractionOutput)
		if !ok {
			t.FailNow()
		}
		secondEmbedding := secondResult.Embeddings[0]
		e := floatsEqual(firstEmbedding, secondEmbedding)
		if e != nil {
			t.Logf("Equality failed for determinism test %s test with pairs %s and %s", k, strings.Join(sentencePair[0], ","), strings.Join(sentencePair[1], ","))
			t.Log("First vector", firstEmbedding)
			t.Log("second vector", secondEmbedding)
			t.Fail()
		}
	}

	zero := uint64(0)
	assert.Greater(t, pipeline.PipelineTimings.NumCalls, zero, "PipelineTimings.NumCalls should be greater than 0")
	assert.Greater(t, pipeline.PipelineTimings.TotalNS, zero, "PipelineTimings.TotalNS should be greater than 0")
	assert.Greater(t, pipeline.TokenizerTimings.NumCalls, zero, "TokenizerTimings.NumCalls should be greater than 0")
	assert.Greater(t, pipeline.TokenizerTimings.TotalNS, zero, "TokenizerTimings.TotalNS should be greater than 0")
}

func TestFeatureExtractionPipelineValidation(t *testing.T) {
	session, err := hugot.NewSession(hugot.WithOnnxLibraryPath(onnxruntimeSharedLibrary))
	check(t, err)
	defer func(session *hugot.Session) {
		err := session.Destroy()
		check(t, err)
	}(session)

	modelFolder := os.Getenv("TEST_MODELS_FOLDER")
	if modelFolder == "" {
		modelFolder = "./models"
	}
	modelPath := path.Join(modelFolder, "all-MiniLM-L6-v2")
	pipeline, err := session.NewFeatureExtractionPipeline(modelPath, "testPipeline")
	check(t, err)

	pipeline.OutputDim = 0
	err = pipeline.Validate()
	assert.Error(t, err)
}

// utilities

// Returns an error if any element between a and b don't match.
func floatsEqual(a, b []float32) error {
	if len(a) != len(b) {
		return fmt.Errorf("length mismatch: %d vs %d", len(a), len(b))
	}
	for i := range a {
		diff := a[i] - b[i]
		if diff < 0 {
			diff = -diff
		}
		// Arbitrarily chosen precision. Large enough not to be affected by quantization
		if diff >= 0.000001 {
			return fmt.Errorf("data element %d doesn't match: %.12f vs %.12f",
				i, a[i], b[i])
		}
	}
	return nil
}

func checkClassificationOutput(t *testing.T, inputResult []pipelines.ClassificationOutput, inputExpected []pipelines.ClassificationOutput) {
	assert.Equal(t, len(inputResult), len(inputExpected))
	for i, output := range inputResult {
		resultExpected := inputExpected[i]
		assert.Equal(t, output.Label, resultExpected.Label)
		assert.True(t, almostEqual(float64(output.Score), float64(resultExpected.Score)))
	}
}

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) <= 0.0001
}

func check(t *testing.T, err error) {
	if err != nil {
		t.Fatalf("Test failed with error %s", err.Error())
	}
}

func printTokenEntities(o *pipelines.TokenClassificationOutput) {
	for i, entities := range o.Entities {
		fmt.Printf("Input %d\n", i)
		for _, entity := range entities {
			fmt.Printf("%+v\n", entity)
		}
	}
}
