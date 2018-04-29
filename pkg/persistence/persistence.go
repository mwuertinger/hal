package persistence

import "sync"

type Service interface {
	Start() error
	Get(key interface{}) (interface{}, error)
	Put(key interface{}, value interface{}) error
}

type inMemoryService struct {
	m sync.Map
}

var inMemoryServiceInstance = inMemoryService{}

func GetInMemoryService() Service {
	return &inMemoryServiceInstance
}

func (s *inMemoryService) Start() error {
	return nil
}

func (s *inMemoryService) Get(key interface{}) (interface{}, error) {
	value, _ := s.m.Load(key)
	return value, nil
}

func (s *inMemoryService) Put(key, value interface{}) error {
	s.m.Store(key, value)
	return nil
}
