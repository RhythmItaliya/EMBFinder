// clip.go — local CLIP stub
//
// The production pipeline uses the Python embedder service (ViT-L-14 on CUDA)
// via callEmbedRaw / callEmbedImageMulti / callEmbedAugmented in handlers.go.
// The Go-native ONNX path was removed — it pulled in onnxruntime_go + x/image
// (~400MB model download) and was never activated in production.
//
// clipReady is kept as a sentinel for the search response JSON field.
package main

const clipReady = false
