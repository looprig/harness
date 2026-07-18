// Package eval is an application-neutral evaluation framework for agentic
// systems. It runs as ordinary Go code under `go test`, reusing the testing
// package for execution, comparison, failure reporting, parallelism, and CI,
// while adding the domain vocabulary the standard library does not provide:
// conversations, expectations, operational evidence, evaluators, rubrics,
// findings, measurements, reports, and sinks.
//
// The framework supports two lifecycles over the same observation and
// assessment contracts. Qualification evaluation runs before deployment
// against fixtures, golden sets, generated cases, models, agents, HTTP
// endpoints, or local processes. Continuous evaluation observes completed
// turns or sessions in production, asynchronously and without altering the
// active conversation.
//
// Evals only observe, score, report, and propose golden-set candidates. They
// never authorize, block, rewrite, retry, or otherwise act on a session.
// Missing evidence and unavailable enforcement are surfaced explicitly as
// unverified, never as a passing score.
//
// An agent interaction is represented as content.AgenticMessages from
// github.com/looprig/core, preserving text, multimodal blocks, tool requests,
// tool results, errors, and usage. The root package depends only on core;
// judge and inference-backed target packages add the inference dependency.
package eval
