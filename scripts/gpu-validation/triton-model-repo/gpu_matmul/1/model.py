"""
gpu_matmul — Triton Python backend model that executes on the GPU.

This is a validation fixture, not a production model.  Its sole purpose is to
produce genuine nv_gpu_utilization > 0 on NVIDIA hardware, proving that the
KIND_GPU instance group forces real CUDA execution and that IFA's Triton adapter
correctly converts the resulting [0,1] fraction to a percentage.

Each request does 20 sequential 128×128 matrix multiplications on CUDA tensors.
The model is fast enough that a single request does not saturate the GPU, but
under concurrent load (the load generator in 01-triton-gpu-validate.sh) GPU
utilization rises measurably.

GPU verification is done at initialize() time: if torch.cuda.is_available()
returns False, the model raises immediately, which causes Triton to report the
model as unavailable — a clear, unambiguous failure, not a silent CPU fallback.
"""

import numpy as np
import triton_python_backend_utils as pb_utils
import torch


class TritonPythonModel:
    def initialize(self, args):
        # Hard failure if no CUDA — no silent CPU fallback.
        if not torch.cuda.is_available():
            raise RuntimeError(
                "gpu_matmul: CUDA not available; this model requires KIND_GPU"
            )
        self.device = torch.device("cuda:0")
        # Warm-up to avoid counting JIT compilation time as inference time.
        warmup = torch.randn(128, 128, device=self.device)
        for _ in range(5):
            warmup = torch.matmul(warmup, warmup)
        torch.cuda.synchronize()

    def execute(self, requests):
        responses = []
        for request in requests:
            inp = pb_utils.get_input_tensor_by_name(request, "INPUT")
            x = torch.from_numpy(inp.as_numpy()).to(self.device)
            # 20 sequential matmuls produce enough GPU activity to register
            # on nv_gpu_utilization under sustained concurrent load.
            result = x
            for _ in range(20):
                result = torch.matmul(result, x)
            torch.cuda.synchronize()
            out_np = result.cpu().numpy().astype(np.float32)
            output_tensor = pb_utils.Tensor("OUTPUT", out_np)
            responses.append(
                pb_utils.InferenceResponse(output_tensors=[output_tensor])
            )
        return responses

    def finalize(self):
        pass
