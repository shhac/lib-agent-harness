//go:build unix

package process

// backgroundNice is the nice value a background tree runs at: well below
// interactive work, still above the idle band.
const backgroundNice = 10
