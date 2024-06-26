package pipelines

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/knights-analytics/tokenizers"
	ort "github.com/yalue/onnxruntime_go"

	util "github.com/knights-analytics/hugot/utils"
)

// BasePipeline is a basic pipeline type used for struct composition in the other pipelines.
type BasePipeline struct {
	ModelPath        string
	OnnxFilename     string
	PipelineName     string
	OrtSession       *ort.DynamicAdvancedSession
	OrtOptions       *ort.SessionOptions
	Tokenizer        *tokenizers.Tokenizer
	TokenizerOptions []tokenizers.EncodeOption
	InputsMeta       []ort.InputOutputInfo
	OutputsMeta      []ort.InputOutputInfo
	hasTokenTypeIds  bool
	hasAttentionMask bool
	OutputDim        int
	TokenizerTimings *Timings
	PipelineTimings  *Timings
}

type PipelineBatchOutput interface {
	GetOutput() []any
}

type Pipeline interface {
	Destroy() error
	GetStats() []string
	GetOutputDim() int
	Validate() error
	Run([]string) (PipelineBatchOutput, error)
}

type PipelineOption[T Pipeline] func(eo T)

type PipelineConfig[T Pipeline] struct {
	ModelPath    string
	Name         string
	OnnxFilename string
	Options      []PipelineOption[T]
}

type Timings struct {
	NumCalls uint64
	TotalNS  uint64
}

type TokenizedInput struct {
	Raw               string
	Tokens            []string
	TokenIds          []uint32
	TypeIds           []uint32
	AttentionMask     []uint32
	SpecialTokensMask []uint32
	MaxAttentionIndex int
	Offsets           []tokenizers.Offset
}

type PipelineBatch struct {
	Input                []TokenizedInput
	IdsTensor            []int64
	TypeIdsTensor        []int64
	AttentionMasksTensor []int64
	MaxSequence          int
	OutputTensor         []float32
}

func (p *BasePipeline) GetOutputDim() int {
	return p.OutputDim
}

func getOnnxFiles(path string) ([][]string, error) {
	var onnxFiles [][]string
	walker := func(_ context.Context, _ string, parent string, info os.FileInfo, _ io.Reader) (toContinue bool, err error) {
		if strings.HasSuffix(info.Name(), ".onnx") {
			onnxFiles = append(onnxFiles, []string{util.PathJoinSafe(path, parent), info.Name()})
		}
		return true, nil
	}
	err := util.FileSystem.Walk(context.Background(), path, walker)
	return onnxFiles, err
}

// Load the ort model supporting the pipeline.
func (p *BasePipeline) loadModel() error {
	tokenizerBytes, err := util.ReadFileBytes(util.PathJoinSafe(p.ModelPath, "tokenizer.json"))
	if err != nil {
		return err
	}

	tk, err := tokenizers.FromBytes(tokenizerBytes)
	if err != nil {
		return err
	}

	// we look for .onnx files.
	var modelOnnxFile string
	onnxFiles, err := getOnnxFiles(p.ModelPath)
	if err != nil {
		return err
	}
	if len(onnxFiles) == 0 {
		return fmt.Errorf("no .onnx file detected at %s. There should be exactly .onnx file", p.ModelPath)
	}
	if len(onnxFiles) > 1 {
		if p.OnnxFilename == "" {
			return fmt.Errorf("multiple .onnx file detected at %s and no OnnxFilename specified", p.ModelPath)
		}
		modelNameFound := false
		for i := range onnxFiles {
			if onnxFiles[i][1] == p.OnnxFilename {
				modelNameFound = true
				modelOnnxFile = util.PathJoinSafe(onnxFiles[i]...)
			}
		}
		if !modelNameFound {
			return fmt.Errorf("file %s not found at %s", p.OnnxFilename, p.ModelPath)
		}
	} else {
		modelOnnxFile = util.PathJoinSafe(onnxFiles[0]...)
	}

	onnxBytes, err := util.ReadFileBytes(modelOnnxFile)
	if err != nil {
		return err
	}

	inputs, outputs, err := ort.GetInputOutputInfoWithONNXData(onnxBytes)
	if err != nil {
		return err
	}

	p.InputsMeta = inputs
	p.OutputsMeta = outputs

	inputNames := make([]string, len(inputs))
	for i, meta := range inputs {
		inputNames[i] = meta.Name
		switch meta.Name {
		case "token_type_ids":
			p.hasTokenTypeIds = true
		case "attention_mask":
			p.hasAttentionMask = true
		}
	}
	outputNames := make([]string, len(outputs))
	for i, meta := range outputs {
		outputNames[i] = meta.Name
	}
	session, err := ort.NewDynamicAdvancedSessionWithONNXData(
		onnxBytes,
		inputNames,
		outputNames,
		p.OrtOptions,
	)
	if err != nil {
		return err
	}

	p.OrtSession = session
	p.Tokenizer = tk
	return nil
}

func (p *BasePipeline) Destroy() error {
	var finalErr error
	errTokenizer := p.Tokenizer.Close()
	if errTokenizer != nil {
		finalErr = errTokenizer
	}
	ortError := p.OrtSession.Destroy()
	if ortError != nil {
		finalErr = ortError
	}
	return finalErr
}

// Preprocess the input strings in the batch
func (p *BasePipeline) Preprocess(inputs []string) PipelineBatch {
	start := time.Now()

	outputs := make([]TokenizedInput, len(inputs))
	maxSequence := 0
	for i, input := range inputs {

		output := p.Tokenizer.EncodeWithOptions(input,
			true,
			p.TokenizerOptions...,
		)

		maxAttentionIndex := 0
		for j, attentionMaskValue := range output.AttentionMask {
			if attentionMaskValue != 0 {
				maxAttentionIndex = j
			}
		}

		outputs[i] = TokenizedInput{
			Raw:               input,
			Tokens:            output.Tokens,
			TokenIds:          output.IDs,
			TypeIds:           output.TypeIDs,
			AttentionMask:     output.AttentionMask,
			MaxAttentionIndex: maxAttentionIndex,
			SpecialTokensMask: output.SpecialTokensMask,
			Offsets:           output.Offsets, // we need the offsets here for postprocessing later
		}
		if maxAttentionIndex > maxSequence {
			maxSequence = maxAttentionIndex
		}
	}

	atomic.AddUint64(&p.TokenizerTimings.NumCalls, 1)
	atomic.AddUint64(&p.TokenizerTimings.TotalNS, uint64(time.Since(start)))
	batch := p.convertInputToTensors(outputs, maxSequence+1)
	return batch
}

func (p *BasePipeline) getInputTensors(batch PipelineBatch, actualBatchSize int64, maxSequence int64) ([]ort.ArbitraryTensor, error) {
	inputTensors := make([]ort.ArbitraryTensor, len(p.InputsMeta))
	var err error

	for i, input := range p.InputsMeta {
		var inputTensor *ort.Tensor[int64]

		// create the tensor for the input name
		switch input.Name {
		case "input_ids":
			inputTensor, err = ort.NewTensor(ort.NewShape(actualBatchSize, maxSequence), batch.IdsTensor)
		case "token_type_ids":
			inputTensor, err = ort.NewTensor(ort.NewShape(actualBatchSize, maxSequence), batch.TypeIdsTensor)
		case "attention_mask":
			inputTensor, err = ort.NewTensor(ort.NewShape(actualBatchSize, maxSequence), batch.AttentionMasksTensor)
		}

		inputTensors[i] = inputTensor
	}
	return inputTensors, err
}

// Forward pass of the neural network on the tokenized input
func (p *BasePipeline) Forward(batch PipelineBatch) (PipelineBatch, error) {
	start := time.Now()

	actualBatchSize := int64(len(batch.Input))
	maxSequence := int64(batch.MaxSequence)
	inputTensors, err := p.getInputTensors(batch, actualBatchSize, maxSequence)
	if err != nil {
		return batch, err
	}

	outputTensor, err4 := ort.NewEmptyTensor[float32](ort.NewShape(actualBatchSize, maxSequence, int64(p.OutputDim)))
	if err4 != nil {
		return batch, err4
	}

	defer func(inputTensors []ort.ArbitraryTensor) {
		for _, tensor := range inputTensors {
			err = errors.Join(err, tensor.Destroy())
		}
	}(inputTensors)

	// Run Onnx model
	errOnnx := p.OrtSession.Run(inputTensors, []ort.ArbitraryTensor{outputTensor})
	if errOnnx != nil {
		return batch, errOnnx
	}
	batch.OutputTensor = outputTensor.GetData()
	defer func(outputTensor *ort.Tensor[float32]) {
		err = errors.Join(err, outputTensor.Destroy())
	}(outputTensor)

	atomic.AddUint64(&p.PipelineTimings.NumCalls, 1)
	atomic.AddUint64(&p.PipelineTimings.TotalNS, uint64(time.Since(start)))
	return batch, err
}

// convert tokenized input to the format required by the onnxruntime library
func (p *BasePipeline) convertInputToTensors(inputs []TokenizedInput, maxSequence int) PipelineBatch {
	tensorSize := len(inputs) * maxSequence
	counter := 0

	idsTensor := make([]int64, tensorSize)
	typeIdsTensor := make([]int64, tensorSize)
	attentionMasksTensor := make([]int64, tensorSize)

	for _, input := range inputs {
		length := len(input.TokenIds)
		for j := 0; j < maxSequence; j++ {
			if j+1 <= length {
				idsTensor[counter] = int64(input.TokenIds[j])
				if p.hasTokenTypeIds {
					typeIdsTensor[counter] = int64(input.TypeIds[j])
				}
				if p.hasAttentionMask {
					attentionMasksTensor[counter] = int64(input.AttentionMask[j])
				}
			} else {
				// padding all vectors to max sequence length
				idsTensor[counter] = 0
				typeIdsTensor[counter] = 0
				attentionMasksTensor[counter] = 0
			}
			counter++
		}
	}
	return PipelineBatch{
		Input:                inputs,
		IdsTensor:            idsTensor,
		TypeIdsTensor:        typeIdsTensor,
		AttentionMasksTensor: attentionMasksTensor,
		MaxSequence:          maxSequence,
	}
}

func (p *BasePipeline) GetStats() []string {
	return []string{
		fmt.Sprintf("Statistics for pipeline: %s", p.PipelineName),
		fmt.Sprintf("Tokenizer: Total time=%s, Execution count=%d, Average query time=%s", time.Duration(p.TokenizerTimings.TotalNS), p.TokenizerTimings.NumCalls, time.Duration(float64(p.TokenizerTimings.TotalNS)/math.Max(1, float64(p.TokenizerTimings.NumCalls)))),
		fmt.Sprintf("ONNX: Total time=%s, Execution count=%d, Average query time=%s", time.Duration(p.PipelineTimings.TotalNS), p.PipelineTimings.NumCalls, time.Duration(float64(p.PipelineTimings.TotalNS)/math.Max(1, float64(p.PipelineTimings.NumCalls)))),
	}
}
