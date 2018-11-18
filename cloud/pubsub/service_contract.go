package pubsub

import (
	"fmt"
	"github.com/viant/toolbox/data"
	"github.com/viant/toolbox/storage"
	"github.com/viant/toolbox/url"
	"io/ioutil"
	"time"
)

const defaultTimeoutMs = 5000

//CreateRequest represents a create resource request
type CreateRequest struct {
	Resources []*Resource
}

func (r *CreateRequest) Init() error {
	if len(r.Resources) == 0 {
		return nil
	}
	for _, resource := range r.Resources {
		if err := resource.Init(); err != nil {
			return err
		}
	}
	return nil
}

func (r *CreateRequest) Validate() error {
	if len(r.Resources) == 0 {
		return fmt.Errorf("resources was empty")
	}
	return nil
}

//CreateResponse represents a create resource response
type CreateResponse struct {
	Resources []*Resource
}

//Config represent a subscription config
type Config struct {
	Topic               *url.Resource
	Labels              map[string]string
	Attributes          map[string]string
	AckDeadline         time.Duration
	RetentionDuration   time.Duration
	RetainAckedMessages bool
}

//NewConfig create new config
func NewConfig(topic string) *Config {
	return &Config{
		Topic: &url.Resource{URL: topic},
	}
}

//Resource represnts pubsub resource
type Resource struct {
	*url.Resource
	Type     string `description:"resource type: topic, subscription"`
	Recreate bool
	Config   *Config
}

//Init initilizes resource
func (r *Resource) Init() error {
	if r.Type == "" {
		if isTopic := r.Config == nil || r.Config.Topic == nil; isTopic {
			r.Type = ResourceTypeTopic
		} else {
			r.Type = ResourceTypeSubscription
		}
	}
	return nil
}

//NewResource creates a new URL
func NewResource(resourceType, URL, credentials string, recreate bool, config *Config) *Resource {
	return &Resource{
		Type:     resourceType,
		Resource: url.NewResource(URL, credentials),
		Recreate: recreate,
		Config:   config,
	}
}

//DeleteRequest represents a delete resource request
type DeleteRequest struct {
	Resources []*Resource
}

//DeleteResponse represents a delete resource response
type DeleteResponse struct{}

//PushRequest represents push request
type PushRequest struct {
	Dest          *url.Resource
	Messages      []*Message
	Source        *url.Resource
	TimeoutMs     int
	UDF           string
	isInitialized bool
}

func (r *PushRequest) Init() error {
	if r.isInitialized {
		return nil
	}
	if r.Source != nil {
		var resource = r.Source
		if err := resource.Init(); err != nil {
			return err
		}
		storageService, err := storage.NewServiceForURL(resource.URL, resource.Credentials)
		if err != nil {
			return err
		}
		object, err := storageService.StorageObject(resource.URL)
		if err != nil {
			return err
		}
		if object.IsFolder() {
			return nil
		}
		reader, err := storageService.Download(object)
		if err != nil {
			return err
		}
		content, err := ioutil.ReadAll(reader)
		if err != nil {
			return err
		}
		r.Messages = loadMessages(content)
	}
	if r.TimeoutMs == 0 {
		r.TimeoutMs = defaultTimeoutMs
	}
	return nil
}

func (r *PushRequest) Validate() error {
	if r.Dest == nil {
		return fmt.Errorf("dest was empty")
	}
	if resource := r.Source; resource != nil {
		storageService, err := storage.NewServiceForURL(resource.URL, resource.Credentials)
		if err != nil {
			return err
		}
		object, err := storageService.StorageObject(resource.URL)
		if err != nil {
			return err
		}
		if object.IsFolder() {
			return fmt.Errorf("resource can not be a folder: " + resource.URL)
		}
	}
	if len(r.Messages) == 0 {
		return fmt.Errorf("messages were empty")
	}
	return nil
}

//PushResponse represents a push response
type PushResponse struct {
	Results []Result
}

//PullRequest represents a pull request
type PullRequest struct {
	Source    *url.Resource
	TimeoutMs int
	Count     int
	UDF       string
}

func (r *PullRequest) Init() error {
	if r.TimeoutMs == 0 {
		r.TimeoutMs = defaultTimeoutMs
	}
	return nil
}

func (r *PullRequest) Validate() error {
	if r.Source == nil {
		return fmt.Errorf("source was empty")
	}
	return nil
}

//PullRequest represents a pull response
type PullResponse struct {
	Messages []*Message
}

type Message struct {
	ID         string
	Attributes map[string]string
	Data       interface{}
}

func (m *Message) Expand(state data.Map) *Message {
	var result = &Message{
		Attributes: make(map[string]string),
	}
	if len(m.Attributes) > 0 {
		for k, v := range m.Attributes {
			result.Attributes[state.ExpandAsText(k)] = state.ExpandAsText(v)
		}
	}
	if m.Data != nil {
		result.Data = state.Expand(m.Data)
	}
	return result
}

type Result interface{}