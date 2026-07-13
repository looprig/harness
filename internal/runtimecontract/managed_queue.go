// Package runtimecontract holds small shared runtime invariants used by otherwise
// independent loop backends. It is internal so these mechanics do not leak into the
// public harness API.
package runtimecontract

// ManagedInputQueueCapacity is the maximum accepted managed inputs waiting behind
// one active turn. Native and foreign actors reject the next input before checked
// durable acceptance.
const ManagedInputQueueCapacity = 64
