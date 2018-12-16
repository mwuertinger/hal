package timer

import (
	"errors"
	"github.com/mwuertinger/hal/pkg/device"
	"log"
	"math/rand"
	"sync"
	"time"
)

type Service interface {
	Start() error
	AddJob(job Job) (uint64, error)
}

type Job struct {
	ID        uint64
	Timestamp time.Time       // execution time
	Switches  []device.Switch // list of switches
	Status    bool            // target status
}

type service struct {
	mu          sync.Mutex // protects everything below
	initialized bool
	jobs        map[uint64]Job
}

func NewService() Service {
	return &service{jobs: map[uint64]Job{}}
}

func (s *service) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.initialized {
		return errors.New("already initialized")
	}

	go func() {
		for now := range time.Tick(time.Minute) {
			s.mu.Lock()
			log.Print("Timer.jobs: ", s.jobs)
			for id, job := range s.jobs {
				if job.Timestamp.Before(now) {
					log.Print("Timer: ", job)
					for _, sw := range job.Switches {
						sw.Switch(job.Status)
					}
					delete(s.jobs, id)
				}
			}
			s.mu.Unlock()
		}
	}()
	return nil
}

func (s *service) AddJob(job Job) (uint64, error) {
	job.ID = rand.Uint64()

	// defensive copying
	switches := make([]device.Switch, len(job.Switches))
	copy(switches, job.Switches)
	job.Switches = switches

	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[job.ID] = job

	return job.ID, nil
}
