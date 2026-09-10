package gbon

// DeriveIfacePtrChainForTest exposes the family grammar to the external
// conformance reader: the spec-corpus buildRoot shares the codec canon
// instead of duplicating it (test-only hook, compiled with the tests).
var DeriveIfacePtrChainForTest = deriveIfacePtrChain
