// src/frame-layout.ts
var FIELDS = [
  "y",
  "cb",
  "cr",
  "width",
  "height",
  "stride_y",
  "stride_c",
  "chroma_width",
  "chroma_height",
  "bit_depth",
  "poc",
  "cvs_id",
  "bytes_per_sample"
];
function parseFrameLayout(spec) {
  const parsed = /* @__PURE__ */ new Map();
  for (const field of spec.split(",")) {
    const sep = field.indexOf(":");
    if (sep < 0) throw new Error(`HEVCFrame layout: malformed field ${JSON.stringify(field)}`);
    const name = field.slice(0, sep);
    const value = Number(field.slice(sep + 1));
    if (!Number.isInteger(value) || value < 0) {
      throw new Error(`HEVCFrame layout: ${name} is not a byte offset (${field})`);
    }
    parsed.set(name, value);
  }
  const offset = {};
  for (const name of FIELDS) {
    const value = parsed.get(name);
    if (value === void 0) throw new Error(`HEVCFrame layout: missing ${name}`);
    offset[name] = value;
  }
  const size = parsed.get("_size");
  if (size === void 0) throw new Error("HEVCFrame layout: missing _size");
  return { offset, size };
}

// src/decoder.ts
var HEVCDecoder = class _HEVCDecoder {
  constructor(module) {
    this._m = module;
    this._api = {
      create: module.cwrap("hevc_decoder_create", "number", []),
      destroy: module.cwrap("hevc_decoder_destroy", null, ["number"]),
      decode: module.cwrap("hevc_decoder_decode", "number", ["number", "number", "number"]),
      getFrameCount: module.cwrap("hevc_decoder_get_frame_count", "number", ["number"]),
      getFrame: module.cwrap("hevc_decoder_get_frame", "number", ["number", "number", "number"]),
      getInfo: module.cwrap("hevc_decoder_get_info", "number", ["number", "number"]),
      feed: module.cwrap("hevc_decoder_feed", "number", ["number", "number", "number"]),
      drain: module.cwrap("hevc_decoder_drain", "number", ["number", "number"]),
      getDrainedFrame: module.cwrap("hevc_decoder_get_drained_frame", "number", ["number", "number", "number"]),
      flush: module.cwrap("hevc_decoder_flush", "number", ["number"]),
      getSuppressedPictureCount: module.cwrap("hevc_decoder_get_suppressed_picture_count", "number", ["number"]),
      takeSuppressedPictures: module.cwrap("hevc_decoder_take_suppressed_pictures", "number", ["number", "number", "number"]),
      frameLayout: module.cwrap("hevc_frame_layout", "string", [])
    };
    this._layout = parseFrameLayout(this._api.frameLayout());
    this._dec = this._api.create();
    if (!this._dec) throw new Error("Failed to create HEVC decoder");
  }
  /**
   * Create a new decoder instance. Loads the WASM module.
   */
  static async create(options) {
    const factoryOpts = {};
    if (options?.wasmBinaryUrl) {
      factoryOpts.locateFile = () => options.wasmBinaryUrl;
    }
    const g = globalThis;
    if (typeof g.HEVCDecoderModule === "function") {
      const module2 = await g.HEVCDecoderModule(factoryOpts);
      return new _HEVCDecoder(module2);
    }
    const wasmUrl = options?.wasmUrl ?? "./wasm/hevc-decode.js";
    const mod = await import(
      /* @vite-ignore */
      wasmUrl
    );
    const fn = mod.default ?? mod;
    const module = await fn(factoryOpts);
    return new _HEVCDecoder(module);
  }
  /**
   * Decode a complete HEVC bitstream.
   * @param data Raw .265 bitstream bytes
   */
  decode(data) {
    const m = this._m;
    const ptr = m._malloc(data.length);
    try {
      m.HEAPU8.set(data, ptr);
      const ret = this._api.decode(this._dec, ptr, data.length);
      if (ret !== 0) throw new Error(`Decode failed (code ${ret})`);
      const count = this._api.getFrameCount(this._dec);
      const frames = [];
      for (let i = 0; i < count; i++) {
        const frame = this._extractFrame(i);
        if (frame) frames.push(frame);
      }
      const info = this._extractInfo();
      return { frames, info };
    } finally {
      m._free(ptr);
    }
  }
  /** Number of decoded frames available */
  get frameCount() {
    return this._api.getFrameCount(this._dec);
  }
  /** Get stream info (available after decode) */
  get info() {
    return this._extractInfo();
  }
  _extractFrame(index) {
    const m = this._m;
    const framePtr = m._malloc(this._layout.size);
    try {
      const ret = this._api.getFrame(this._dec, index, framePtr);
      if (ret !== 0) return null;
      return this._readFrameFromPtr(framePtr);
    } finally {
      m._free(framePtr);
    }
  }
  _extractDrainedFrame(index) {
    const m = this._m;
    const framePtr = m._malloc(this._layout.size);
    try {
      const ret = this._api.getDrainedFrame(this._dec, index, framePtr);
      if (ret !== 0) return null;
      return this._readFrameFromPtr(framePtr);
    } finally {
      m._free(framePtr);
    }
  }
  _readFrameFromPtr(framePtr) {
    const m = this._m;
    const at = this._layout.offset;
    const yPtr = m.getValue(framePtr + at.y, "*");
    const cbPtr = m.getValue(framePtr + at.cb, "*");
    const crPtr = m.getValue(framePtr + at.cr, "*");
    const width = m.getValue(framePtr + at.width, "i32");
    const height = m.getValue(framePtr + at.height, "i32");
    const strideY = m.getValue(framePtr + at.stride_y, "i32");
    const strideC = m.getValue(framePtr + at.stride_c, "i32");
    const cw = m.getValue(framePtr + at.chroma_width, "i32");
    const ch = m.getValue(framePtr + at.chroma_height, "i32");
    const bd = m.getValue(framePtr + at.bit_depth, "i32");
    const poc = m.getValue(framePtr + at.poc, "i32");
    const cvsId = m.getValue(framePtr + at.cvs_id, "i32");
    const bps = m.getValue(framePtr + at.bytes_per_sample, "i32");
    const y = copyPlane(m, yPtr, width, height, strideY, bps);
    const cb = copyPlane(m, cbPtr, cw, ch, strideC, bps);
    const cr = copyPlane(m, crPtr, cw, ch, strideC, bps);
    return { y, cb, cr, width, height, chromaWidth: cw, chromaHeight: ch, bitDepth: bd, poc, cvsId };
  }
  _extractInfo() {
    const m = this._m;
    const infoPtr = m._malloc(24);
    try {
      const ret = this._api.getInfo(this._dec, infoPtr);
      if (ret !== 0) return null;
      return {
        width: m.getValue(infoPtr, "i32"),
        height: m.getValue(infoPtr + 4, "i32"),
        bitDepth: m.getValue(infoPtr + 8, "i32"),
        chromaFormat: m.getValue(infoPtr + 12, "i32"),
        profile: m.getValue(infoPtr + 16, "i32"),
        level: m.getValue(infoPtr + 20, "i32")
      };
    } finally {
      m._free(infoPtr);
    }
  }
  // --- Incremental API (streaming) ---
  /**
   * Feed a chunk of data containing one or more complete NAL units.
   * The decoder accumulates parameter sets and decodes pictures incrementally.
   * Call drain() after each feed() to retrieve output-ready frames.
   */
  feed(data) {
    const m = this._m;
    const ptr = m._malloc(data.length);
    try {
      m.HEAPU8.set(data, ptr);
      const ret = this._api.feed(this._dec, ptr, data.length);
      if (ret !== 0) throw new Error(`Feed failed (code ${ret})`);
    } finally {
      m._free(ptr);
    }
  }
  /**
   * Drain output-ready frames from the decoder (§C.5.2 bumping process).
   * Returns frames in display order, only when ready per DPB constraints.
   *
   * Planes are copied out of the WASM heap, so the returned frames stay
   * valid across later feed() and destroy() calls. Draining after every
   * feed() is what keeps the DPB at its bound — the decoder cannot reclaim
   * a picture before it has been bumped out. See docs/memory-envelope.md.
   */
  drain() {
    const m = this._m;
    const countPtr = m._malloc(4);
    try {
      const ret = this._api.drain(this._dec, countPtr);
      if (ret !== 0) return [];
      const count = m.getValue(countPtr, "i32");
      const frames = [];
      for (let i = 0; i < count; i++) {
        const frame = this._extractDrainedFrame(i);
        if (frame) frames.push(frame);
      }
      return frames;
    } finally {
      m._free(countPtr);
    }
  }
  /**
   * Flush all remaining frames from the DPB (call at end of stream).
   * Returns all buffered frames in display order.
   */
  flush() {
    const ret = this._api.flush(this._dec);
    if (ret !== 0) return [];
    const m = this._m;
    const countPtr = m._malloc(4);
    try {
      const frames = [];
      const framePtr = m._malloc(this._layout.size);
      try {
        for (let i = 0; ; i++) {
          const r = this._api.getDrainedFrame(this._dec, i, framePtr);
          if (r !== 0) break;
          frames.push(this._readFrameFromPtr(framePtr));
        }
      } finally {
        m._free(framePtr);
      }
      return frames;
    } finally {
      m._free(countPtr);
    }
  }
  /**
   * The pictures decoded since the last call that will never be output:
   * PicOutputFlag was 0 (§C.3.1, §8.1), or a new sequence emptied them from
   * the DPB without output (§C.5.2.2). Empties the list.
   *
   * A caller that maps output frames onto per-sample timestamps needs these:
   * the sample that carried such a picture produces no frame, and without
   * knowing which one it was, every later frame of the segment takes the
   * timestamp of its predecessor. Each one comes with its CVS, since POC
   * restarts at every IRAP and alone cannot order pictures across one.
   */
  takeSuppressedPictures() {
    const m = this._m;
    const count = this._api.getSuppressedPictureCount(this._dec);
    if (count <= 0) return [];
    const ptr = m._malloc(count * 8);
    try {
      const written = this._api.takeSuppressedPictures(this._dec, ptr, count);
      if (written < 0) return [];
      const out = [];
      for (let i = 0; i < written; i++) {
        out.push({
          cvsId: m.getValue(ptr + i * 8, "i32"),
          poc: m.getValue(ptr + i * 8 + 4, "i32")
        });
      }
      return out;
    } finally {
      m._free(ptr);
    }
  }
  /** Release decoder resources */
  destroy() {
    if (this._dec) {
      this._api.destroy(this._dec);
      this._dec = 0;
    }
  }
};
function copyPlane(m, ptr, width, height, stride, bytesPerSample) {
  const out = new Uint16Array(width * height);
  const heap = bytesPerSample === 1 ? m.HEAPU8 : m.HEAPU16;
  const base = bytesPerSample === 1 ? ptr : ptr >> 1;
  for (let y = 0; y < height; y++) {
    out.set(heap.subarray(base + y * stride, base + y * stride + width), y * width);
  }
  return out;
}

export {
  HEVCDecoder
};
//# sourceMappingURL=chunk-M6WO4WIK.js.map