package encoder

import (
	"bytes"
	"image"
	"image/jpeg"
	"sync"
)

const maxPooledFrameBytes = 32 * 1024 * 1024

// YUV420Buffer stores planar I420 (Y, U, V) frame planes sized for even dimensions.
type YUV420Buffer struct {
	Width   int
	Height  int
	Y       []byte
	U       []byte
	V       []byte
	YStride int
	UStride int
	VStride int
}

// FramePool recycles RGBA and YUV420p frame buffers via sync.Pool to eliminate GC pressure (NFR-03).
type FramePool struct {
	rgbaPool sync.Pool
	yuvPool  sync.Pool
}

// NewFramePool creates a ready-to-use buffer pool for video capture and encoding.
func NewFramePool() *FramePool {
	return &FramePool{}
}

// GetRGBA returns a pooled *image.RGBA sized to w x h.
func (p *FramePool) GetRGBA(w, h int) *image.RGBA {
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	needed := w * h * 4
	if p != nil {
		if v := p.rgbaPool.Get(); v != nil {
			img := v.(*image.RGBA)
			if cap(img.Pix) >= needed {
				img.Pix = img.Pix[:needed]
				img.Stride = w * 4
				img.Rect = image.Rect(0, 0, w, h)
				return img
			}
		}
	}
	return image.NewRGBA(image.Rect(0, 0, w, h))
}

// PutRGBA returns an *image.RGBA buffer to the pool.
func (p *FramePool) PutRGBA(img *image.RGBA) {
	if p == nil || img == nil || cap(img.Pix) > maxPooledFrameBytes {
		return
	}
	p.rgbaPool.Put(img)
}

// GetYUV420 returns a pooled planar I420 buffer sized to even w x h dimensions.
func (p *FramePool) GetYUV420(w, h int) *YUV420Buffer {
	w = evenDim(w)
	h = evenDim(h)
	ySize := w * h
	uvW := w / 2
	uvH := h / 2
	uvSize := uvW * uvH

	if p != nil {
		if v := p.yuvPool.Get(); v != nil {
			buf := v.(*YUV420Buffer)
			if cap(buf.Y) >= ySize && cap(buf.U) >= uvSize && cap(buf.V) >= uvSize {
				buf.Width = w
				buf.Height = h
				buf.YStride = w
				buf.UStride = uvW
				buf.VStride = uvW
				buf.Y = buf.Y[:ySize]
				buf.U = buf.U[:uvSize]
				buf.V = buf.V[:uvSize]
				return buf
			}
		}
	}

	return &YUV420Buffer{
		Width:   w,
		Height:  h,
		YStride: w,
		UStride: uvW,
		VStride: uvW,
		Y:       make([]byte, ySize),
		U:       make([]byte, uvSize),
		V:       make([]byte, uvSize),
	}
}

// PutYUV420 returns a YUV420Buffer to the pool.
func (p *FramePool) PutYUV420(buf *YUV420Buffer) {
	if p == nil || buf == nil || cap(buf.Y) > maxPooledFrameBytes {
		return
	}
	p.yuvPool.Put(buf)
}

// WritePlanar concatenates Y, U, and V planes contiguously into dst, resizing dst if needed.
func (b *YUV420Buffer) WritePlanar(dst []byte) []byte {
	total := len(b.Y) + len(b.U) + len(b.V)
	if cap(dst) < total {
		dst = make([]byte, total)
	} else {
		dst = dst[:total]
	}
	off := copy(dst, b.Y)
	off += copy(dst[off:], b.U)
	copy(dst[off:], b.V)
	return dst
}

// ConvertRGBAToYUV420 scales src to dstW x dstH (rounded to even) and converts RGBA to planar I420.
func ConvertRGBAToYUV420(src *image.RGBA, dstW, dstH int, pool *FramePool) *YUV420Buffer {
	dstW = evenDim(dstW)
	dstH = evenDim(dstH)
	yuv := pool.GetYUV420(dstW, dstH)
	if src == nil || src.Rect.Empty() {
		clear(yuv.Y)
		for i := range yuv.U {
			yuv.U[i] = 128
			yuv.V[i] = 128
		}
		return yuv
	}

	srcW := src.Rect.Dx()
	srcH := src.Rect.Dy()
	baseOff := src.PixOffset(src.Rect.Min.X, src.Rect.Min.Y)
	pix := src.Pix
	stride := src.Stride

	// Fast path when no scaling is needed.
	sameSize := srcW == dstW && srcH == dstH
	uvW := dstW / 2

	for dy := 0; dy < dstH; dy += 2 {
		sy0 := dy
		sy1 := dy + 1
		if !sameSize {
			sy0 = (dy * srcH) / dstH
			sy1 = ((dy + 1) * srcH) / dstH
			if sy1 >= srcH {
				sy1 = srcH - 1
			}
		}
		row0 := baseOff + sy0*stride
		row1 := baseOff + sy1*stride
		yRow0 := dy * yuv.YStride
		yRow1 := (dy + 1) * yuv.YStride
		uvRow := (dy / 2) * uvW

		for dx := 0; dx < dstW; dx += 2 {
			sx0 := dx
			sx1 := dx + 1
			if !sameSize {
				sx0 = (dx * srcW) / dstW
				sx1 = ((dx + 1) * srcW) / dstW
				if sx1 >= srcW {
					sx1 = srcW - 1
				}
			}

			p00 := row0 + sx0*4
			p01 := row0 + sx1*4
			p10 := row1 + sx0*4
			p11 := row1 + sx1*4

			r00, g00, b00 := int(pix[p00]), int(pix[p00+1]), int(pix[p00+2])
			r01, g01, b01 := int(pix[p01]), int(pix[p01+1]), int(pix[p01+2])
			r10, g10, b10 := int(pix[p10]), int(pix[p10+1]), int(pix[p10+2])
			r11, g11, b11 := int(pix[p11]), int(pix[p11+1]), int(pix[p11+2])

			yuv.Y[yRow0+dx] = clampByte(((66*r00 + 129*g00 + 25*b00 + 128) >> 8) + 16)
			yuv.Y[yRow0+dx+1] = clampByte(((66*r01 + 129*g01 + 25*b01 + 128) >> 8) + 16)
			yuv.Y[yRow1+dx] = clampByte(((66*r10 + 129*g10 + 25*b10 + 128) >> 8) + 16)
			yuv.Y[yRow1+dx+1] = clampByte(((66*r11 + 129*g11 + 25*b11 + 128) >> 8) + 16)

			rAvg := (r00 + r01 + r10 + r11 + 2) >> 2
			gAvg := (g00 + g01 + g10 + g11 + 2) >> 2
			bAvg := (b00 + b01 + b10 + b11 + 2) >> 2

			yuv.U[uvRow+dx/2] = clampByte(((-38*rAvg - 74*gAvg + 112*bAvg + 128) >> 8) + 128)
			yuv.V[uvRow+dx/2] = clampByte(((112*rAvg - 94*gAvg - 18*bAvg + 128) >> 8) + 128)
		}
	}

	return yuv
}

// ConvertYUV420ToRGBA converts planar I420 buffers back into a pooled *image.RGBA.
func ConvertYUV420ToRGBA(yuv *YUV420Buffer, pool *FramePool) *image.RGBA {
	if yuv == nil || yuv.Width <= 0 || yuv.Height <= 0 {
		return nil
	}
	out := pool.GetRGBA(yuv.Width, yuv.Height)
	for y := 0; y < yuv.Height; y++ {
		yOff := y * yuv.YStride
		uvOff := (y / 2) * yuv.UStride
		dstOff := y * out.Stride
		for x := 0; x < yuv.Width; x++ {
			yy := int(yuv.Y[yOff+x]) - 16
			uu := int(yuv.U[uvOff+x/2]) - 128
			vv := int(yuv.V[uvOff+x/2]) - 128

			c := 298 * yy
			r := (c + 409*vv + 128) >> 8
			g := (c - 100*uu - 208*vv + 128) >> 8
			b := (c + 516*uu + 128) >> 8

			p := dstOff + x*4
			out.Pix[p] = clampByte(r)
			out.Pix[p+1] = clampByte(g)
			out.Pix[p+2] = clampByte(b)
			out.Pix[p+3] = 255
		}
	}
	return out
}

// DownscaleRGBA returns a pooled *image.RGBA scaled down proportionally within maxW x maxH.
func DownscaleRGBA(src image.Image, maxW, maxH int, pool *FramePool) *image.RGBA {
	if src == nil || src.Bounds().Empty() {
		return nil
	}
	b := src.Bounds()
	srcW, srcH := b.Dx(), b.Dy()
	dstW, dstH := srcW, srcH
	if maxW > 0 && maxH > 0 && (srcW > maxW || srcH > maxH) {
		scaleW := float64(maxW) / float64(srcW)
		scaleH := float64(maxH) / float64(srcH)
		scale := scaleW
		if scaleH < scaleW {
			scale = scaleH
		}
		dstW = max(2, int(float64(srcW)*scale+0.5))
		dstH = max(2, int(float64(srcH)*scale+0.5))
	}
	dst := pool.GetRGBA(dstW, dstH)
	if rgbaSrc, ok := src.(*image.RGBA); ok {
		base := rgbaSrc.PixOffset(b.Min.X, b.Min.Y)
		for y := 0; y < dstH; y++ {
			sy := (y * srcH) / dstH
			sRow := base + sy*rgbaSrc.Stride
			dRow := y * dst.Stride
			for x := 0; x < dstW; x++ {
				sx := (x * srcW) / dstW
				sp := sRow + sx*4
				dp := dRow + x*4
				dst.Pix[dp] = rgbaSrc.Pix[sp]
				dst.Pix[dp+1] = rgbaSrc.Pix[sp+1]
				dst.Pix[dp+2] = rgbaSrc.Pix[sp+2]
				dst.Pix[dp+3] = 255
			}
		}
		return dst
	}

	for y := 0; y < dstH; y++ {
		sy := b.Min.Y + (y*srcH)/dstH
		dRow := y * dst.Stride
		for x := 0; x < dstW; x++ {
			sx := b.Min.X + (x*srcW)/dstW
			r, g, bl, _ := src.At(sx, sy).RGBA()
			dp := dRow + x*4
			dst.Pix[dp] = uint8(r >> 8)
			dst.Pix[dp+1] = uint8(g >> 8)
			dst.Pix[dp+2] = uint8(bl >> 8)
			dst.Pix[dp+3] = 255
		}
	}
	return dst
}

// EncodeRGBAToJPEG encodes an image into a compact JPEG byte slice.
func EncodeRGBAToJPEG(img image.Image, quality int) ([]byte, error) {
	if quality <= 0 || quality > 100 {
		quality = 75
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func evenDim(v int) int {
	if v <= 2 {
		return 2
	}
	return v &^ 1
}

func clampByte(v int) uint8 {
	if v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	return uint8(v)
}
