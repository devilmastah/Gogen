// Package codeindex is the working folder's vector index.
//
// The index lives at .gogen/index. It does not create that directory until a
// refresh publishes a complete index. Search is cosine similarity over the
// stored vectors. At 20,000 chunks or more, that lookup may be an in-memory
// structure built from those vectors, and it is rebuilt when the stored
// vectors change. The package does not import the agent or a host; callers
// pass an Embedder.
package codeindex
