//go:build aievallive

package main

// liveBuilt: this build runs the live suite (-suite live): brant-worker -agent -model openaicompat
// against the model service in the environment (decisions E-3, E-5; make ai-eval-live). Never in CI.
const liveBuilt = true
