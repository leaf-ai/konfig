package konfig

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// ErrNoLoaders is the error returned when no loaders are set in the config and Load is called
var ErrNoLoaders = errors.New("No loaders in config")

// Loader is the interface a config loader must implement to be used withint the package
type Loader interface {
	// StopOnFailure tells wether a loader failure should lead to closing config and the registered closers.
	StopOnFailure() bool
	// Name returns the name of the loader
	Name() string
	// Load loads config values in a Values
	Load(Values) error
	// MaxRetry returns the max number of times to retry when Load fails
	MaxRetry() int
	// RetryDelay returns the delay between each retry
	RetryDelay() time.Duration
}

// LoaderHooks are functions ran when a config load has been performed
type LoaderHooks []func(Store) error

// Run runs all loader and stops when it encounters an error
func (l LoaderHooks) Run(cfg Store) error {
	for _, h := range l {
		if err := h(cfg); err != nil {
			return err
		}
	}
	return nil
}

// LoadWatch loads the config then starts watching it
func LoadWatch() error {
	return instance().LoadWatch()
}
func (c *store) LoadWatch() error {
	if err := c.Load(); err != nil {
		return err
	} else if err := c.Watch(); err != nil {
		return err
	}
	return nil
}

// Load is a function running load on the global config instance
func Load() error {
	return instance().Load()
}

func (c *store) Load() error {
	if len(c.WatcherLoaders) == 0 {
		panic(ErrNoLoaders)
	}
	for _, l := range c.WatcherLoaders {
		// we load the loader once, then we start the reload worker with the watcher
		if err := c.loaderLoadRetry(l, 0); err != nil {

			// if loader says we should stop in failure, stop the world
			// else just return the error
			if l.StopOnFailure() {
				c.stop()
			}

			return err
		}
	}

	// now that we've loaded everything, let's check strict keys
	if err := c.checkStrictKeys(); err != nil {
		c.cfg.Logger.Get().Error("Error while checking strict keys: " + err.Error())
		return err
	}
	c.loaded = true

	return nil
}

// ConfigLoader is a wrapper of Loader with methods to add hooks
type ConfigLoader struct {
	*loaderWatcher
	mut *sync.Mutex
}

func (c *store) newConfigLoader(lw *loaderWatcher) *ConfigLoader {
	var cl = &ConfigLoader{
		loaderWatcher: lw,
		mut:           &sync.Mutex{},
	}

	return cl
}

// AddHooks adds hooks to the loader
func (cl *ConfigLoader) AddHooks(loaderHooks ...func(Store) error) *ConfigLoader {
	cl.mut.Lock()
	defer cl.mut.Unlock()

	if cl.loaderWatcher.loaderHooks == nil {
		cl.loaderWatcher.loaderHooks = make(LoaderHooks, 0)
	}

	cl.loaderWatcher.loaderHooks = append(
		cl.loaderWatcher.loaderHooks,
		loaderHooks...,
	)

	return cl
}

// We don't look for Done on the watcher here as the NopWatcher needs to run load at least once
func (c *store) loaderLoadRetry(wl *loaderWatcher, retry int) error {
	// we create a new Values
	var v = make(Values, len(wl.values))

	// we call the loader
	if err := wl.Load(v); err != nil {

		if retry >= wl.MaxRetry() {
			c.cfg.Logger.Get().Error(err.Error())
			return err
		}

		// wait before retrying
		time.Sleep(wl.RetryDelay())

		return c.loaderLoadRetry(wl, retry+1)
	}

	// we add the values to the store
	v.load(wl.values, c)
	wl.values = v

	// if we have strict keys setup on the store and we have already loaded configs
	// we check those keys now, if they are not present, we will return the error.
	if c.strictKeys != nil && c.loaded {
		if err := c.checkStrictKeys(); err != nil {
			c.cfg.Logger.Get().Error("Error while checking strict keys: " + err.Error())
			return err
		}
	}

	// we run the hooks
	if wl.loaderHooks != nil {
		c.mut.Lock()
		if err := wl.loaderHooks.Run(c); err != nil {
			c.cfg.Logger.Get().Error("Error while running loader hooks: " + err.Error())
			c.mut.Unlock()
			return err
		}
		c.mut.Unlock()
	}

	return nil
}

func (c *store) watchLoader(wl *loaderWatcher) {
	// if a panic occurs close everything
	defer func() {
		if r := recover(); r != nil {
			c.cfg.Logger.Get().Error(fmt.Sprintf("%v", r))
			c.stop()
			return
		}
	}()

	// make sure we recover from panics
	for {
		select {
		case <-wl.Done():
			if err := wl.Err(); err != nil {
				c.cfg.Logger.Get().Error(err.Error())
			}
			// the watcher is closed
			return
		case <-wl.Watch():
			// we got an event
			// do a loaderLoadRetry
			select {
			case <-wl.Done():
				if err := wl.Err(); err != nil {
					c.cfg.Logger.Get().Error(err.Error())
				}
				return
			default:

				var t *prometheus.Timer
				if c.cfg.Metrics {
					t = prometheus.NewTimer(wl.metrics.configReloadDuration)
				}

				if err := c.loaderLoadRetry(wl, 0); err != nil {
					// if metrics is enabled we record a load failure
					if c.cfg.Metrics {
						wl.metrics.configReloadFailure.Inc()
						t.ObserveDuration()
					}
					if !wl.StopOnFailure() {
						continue
					}
					c.stop()
					return
				}

				if c.cfg.Metrics {
					t.ObserveDuration()
					wl.metrics.configReloadSuccess.Inc()
				}
			}
		}
	}
}
