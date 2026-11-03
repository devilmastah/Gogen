// Package codeindex is the working folder's vector index.
//
// The index lives at .gogen/index. Open reads only meta.json, so it does not
// create the directory and does not load the vectors; the first search or
// rebuild loads them. A refresh creates the directory while it runs and
// removes the files it created when it fails. Search is cosine similarity
// over the stored vectors. At 20,000 chunks or more, that lookup may be an
// in-memory structure built from those vectors, and it is rebuilt when the
// stored generation changes. The package does not import the agent or a
// host; callers pass an Embedder.
package codeindex
