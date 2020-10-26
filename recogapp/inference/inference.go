package inference

import (
	"bufio"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path"
	"sort"

	tf "github.com/tensorflow/tensorflow/tensorflow/go"
	"github.com/tensorflow/tensorflow/tensorflow/go/op"
	"gopkg.in/yaml.v2"
)

// Config 이미지 추론 모델 생성 설정정보
type Config struct {
	ModelsPath    string
	UserModelPath string
}

// Inference 이미지 추론 모델 관리
type Inference struct {
	models        map[string]iModel
	modelsPath    string
	userModelPath string
}

type modelConfig struct {
	Name                string   `yaml:"name"`
	Tags                []string `yaml:"tags"`
	InputShape          []int32  `yaml:"input_shape"`
	InputOperationName  string   `yaml:"input_operation_name"`
	OutputOperationName string   `yaml:"output_operation_name"`
	LabelsFile          string   `yaml:"labels_file"`
	Description         string   `yaml:"description"`
}

func loadModel(modelPath string) (iModel, error) {
	var (
		cfgBytes []byte
		cfg      modelConfig
		model    *tf.SavedModel
		labelsFp *os.File
		labels   []string
		err      error
	)

	// config 로드
	cfgFile := path.Join(modelPath, "config.yaml")
	if cfgBytes, err = ioutil.ReadFile(cfgFile); err != nil {
		return iModel{}, err
	}

	if err := yaml.Unmarshal(cfgBytes, &cfg); err != nil {
		return iModel{}, err
	}

	// model 로드
	if model, err = tf.LoadSavedModel(modelPath, cfg.Tags, nil); err != nil {
		return iModel{}, err
	}

	// labels 로드
	labelsFile := path.Join(modelPath, cfg.LabelsFile)
	if labelsFp, err = os.Open(labelsFile); err != nil {
		return iModel{}, err
	}
	defer labelsFp.Close()

	scanner := bufio.NewScanner(labelsFp)
	for scanner.Scan() {
		labels = append(labels, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return iModel{}, err
	}

	m := iModel{
		name:         cfg.Name,
		model:        model,
		inputOp:      cfg.InputOperationName,
		outputOp:     cfg.OutputOperationName,
		inputShape:   cfg.InputShape,
		imageDecoder: make(map[string]imageDecode),
		nrLables:     len(labels),
		labels:       labels,
		desc:         cfg.Description,
	}

	return m, nil
}

func (i *Inference) loadModels() error {
	dirs, _ := ioutil.ReadDir(i.modelsPath)

	for _, dir := range dirs {
		modelPath := path.Join(i.modelsPath, dir.Name())

		if m, err := loadModel(modelPath); err != nil {
			log.Printf("Fail to load model(%s): %s", modelPath, err)
		} else {
			i.addModel(m)
		}
	}

	if i.userModelPath != "" {
		if m, err := loadModel(i.userModelPath); err != nil {
			log.Printf("Fail to load user model(%s): %s", i.userModelPath, err)
		} else {
			i.addModel(m)
		}
	}

	return nil
}

func (i *Inference) addModel(m iModel) {
	for modelName := range i.models {
		if modelName == m.name {
			log.Printf("Duplicated model: %s", m.name)
			return
		}
	}

	i.models[m.name] = m
	log.Printf("Successfully loaded: %s", m.name)
}

// GetModels 이미지 추론 모델 목록 반환
func (i *Inference) GetModels() []string {
	var models []string
	for model := range i.models {
		models = append(models, model)
	}

	return models
}

// GetModel 이미지 추론 모델 정보 반환
func (i *Inference) GetModel(model string) map[string]interface{} {
	if m, ok := i.models[model]; ok {
		return map[string]interface{}{
			"Model":            m.name,
			"Input operator":   m.inputOp,
			"Output operator":  m.outputOp,
			"Number of lables": len(m.labels),
			"Description":      m.desc,
		}
	}

	return nil
}

// Infer 추론
func (i *Inference) Infer(model, image, format string, k int) ([]InferLabel, error) {
	m, ok := i.models[model]
	if !ok {
		return nil, fmt.Errorf("Cannot find model: %s", model)
	}

	result, err := m.infer(image, format)
	if err != nil {
		return nil, err
	}

	var infers []InferLabel
	for idx, prob := range result {
		if idx >= len(m.labels) {
			break
		}

		infers = append(infers, InferLabel{
			Prob:  prob,
			Label: m.labels[idx],
		})
	}
	sort.Sort(sortByProb(infers))

	if k <= 0 {
		k = 5
	}

	return infers[:k], nil
}

// Model 이미지 추론 모델
type iModel struct {
	name string

	model      *tf.SavedModel
	inputOp    string
	outputOp   string
	inputShape []int32

	imageDecoder map[string]imageDecode

	nrLables int
	labels   []string

	desc string
}

// 이미지 타입의 디코더
type imageDecode struct {
	graph   *tf.Graph
	session *tf.Session
	input   tf.Output
	output  tf.Output
}

func (m *iModel) infer(image, format string) ([]float32, error) {
	var (
		inputImage *tf.Tensor
		results    []*tf.Tensor
		err        error
	)

	if inputImage, err = m.normInputImage(image, format); err != nil {
		return nil, err
	}

	if results, err = m.model.Session.Run(
		map[tf.Output]*tf.Tensor{
			m.model.Graph.Operation(m.inputOp).Output(0): inputImage,
		},
		[]tf.Output{
			m.model.Graph.Operation(m.outputOp).Output(0),
		},
		nil,
	); err != nil {
		return nil, err
	}

	return results[0].Value().([][]float32)[0], nil
}

func (m *iModel) normInputImage(image, format string) (*tf.Tensor, error) {
	var (
		decoder     imageDecode
		imageTensor *tf.Tensor
		norms       []*tf.Tensor
		err         error
	)

	if decoder, err = m.getImageDecoder(format); err != nil {
		return nil, err
	}

	if imageTensor, err = tf.NewTensor(image); err != nil {
		return nil, err
	}

	if norms, err = decoder.session.Run(
		map[tf.Output]*tf.Tensor{
			decoder.input: imageTensor,
		},
		[]tf.Output{
			decoder.output,
		},
		nil,
	); err != nil {
		return nil, err
	}

	return norms[0], nil
}

func (m *iModel) getImageDecoder(format string) (imageDecode, error) {
	var (
		decode  tf.Output
		session *tf.Session
		graph   *tf.Graph
		err     error
	)

	if decoder, ok := m.imageDecoder[format]; ok {
		return decoder, nil
	}

	scope := op.NewScope()
	input := op.Placeholder(scope, tf.String)

	if format == "jpg" || format == "jpeg" {
		decode = op.DecodeJpeg(scope, input, op.DecodeJpegChannels(3))
	} else if format == "png" {
		decode = op.DecodePng(scope, input, op.DecodePngChannels(3))
	} else {
		return imageDecode{}, fmt.Errorf("Unsupported image format: %s", format)
	}

	output := op.Div(scope,
		op.Sub(scope,
			op.ResizeBilinear(scope,
				op.ExpandDims(scope,
					op.Cast(scope, decode, tf.Float),
					op.Const(scope.SubScope("make_batch"), int32(0))),
				op.Const(scope.SubScope("size"), m.inputShape)),
			op.Const(scope.SubScope("mean"), float32(117))),
		op.Const(scope.SubScope("scale"), float32(1)))

	if graph, err = scope.Finalize(); err != nil {
		return imageDecode{}, err
	}

	if session, err = tf.NewSession(graph, nil); err != nil {
		return imageDecode{}, err
	}

	decoder := imageDecode{
		graph:   graph,
		input:   input,
		output:  output,
		session: session,
	}
	m.imageDecoder[format] = decoder

	return decoder, nil
}

// InferLabel 이미지 추론 항목
type InferLabel struct {
	Prob  float32
	Label string
}

type sortByProb []InferLabel

func (s sortByProb) Len() int {
	return len(s)
}

func (s sortByProb) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s sortByProb) Less(i, j int) bool {
	return s[i].Prob > s[j].Prob
}

// New 이미지 추론 모델 생성
func New(c Config) (i *Inference, err error) {
	i = &Inference{
		models:        make(map[string]iModel),
		modelsPath:    c.ModelsPath,
		userModelPath: c.UserModelPath,
	}
	err = i.loadModels()

	return
}