import { useRef, useMemo } from "react";
import { Canvas, useFrame } from "@react-three/fiber";
import {
  Sphere,
  MeshDistortMaterial,
  Environment,
  Float,
} from "@react-three/drei";
import * as THREE from "three";

interface AgentOrbProps {
  isPlaying: boolean;
  isConnected: boolean;
}

function GlowingSphere({ isPlaying, isConnected }: AgentOrbProps) {
  const meshRef = useRef<THREE.Mesh>(null);
  const matRef = useRef<any>(null);

  // Base color based on state
  const color = useMemo(() => {
    if (isPlaying) return "#10b981"; // Emerald-500
    if (isConnected) return "#3b82f6"; // Blue-500
    return "#475569"; // Slate-600
  }, [isPlaying, isConnected]);

  useFrame((state) => {
    if (!meshRef.current) return;

    // Pulse effect
    const time = state.clock.getElapsedTime();
    const speed = isPlaying ? 2 : 0.5;

    // Scale pulsing
    const baseScale = 0.55;
    const scale = baseScale + Math.sin(time * speed) * 0.03;
    meshRef.current.scale.set(scale, scale, scale);

    // Subtle rotation
    meshRef.current.rotation.x = time * 0.2;
    meshRef.current.rotation.y = time * 0.3;

    // Animate material distortion
    if (matRef.current) {
      const targetDistort = isPlaying ? 0.6 : 0.3;
      matRef.current.distort = THREE.MathUtils.lerp(
        matRef.current.distort,
        targetDistort,
        0.1
      );

      const targetSpeed = isPlaying ? 5 : 2;
      matRef.current.speed = THREE.MathUtils.lerp(
        matRef.current.speed,
        targetSpeed,
        0.1
      );
    }
  });

  return (
    <Float speed={2} rotationIntensity={0.5} floatIntensity={0.5}>
      <Sphere args={[1, 64, 64]} ref={meshRef}>
        <MeshDistortMaterial
          ref={matRef}
          color={color}
          emissive={color}
          emissiveIntensity={0.5}
          envMapIntensity={0.2}
          clearcoat={0.5}
          clearcoatRoughness={0.2}
          metalness={0.8}
          roughness={0.4}
          distort={0.3} // Initial value
          speed={2} // Initial value
        />
      </Sphere>
      {/* Glow Halo */}
      <mesh scale={[0.7, 0.7, 0.7]}>
        <sphereGeometry args={[1, 32, 32]} />
        <meshBasicMaterial
          color={color}
          transparent
          opacity={0.2}
          blending={THREE.AdditiveBlending}
          side={THREE.BackSide}
        />
      </mesh>
    </Float>
  );
}

export default function AgentOrb(props: AgentOrbProps) {
  return (
    <div className="w-full h-full">
      <Canvas camera={{ position: [0, 0, 3], fov: 45 }}>
        <ambientLight intensity={0.5} />
        <pointLight position={[10, 10, 10]} intensity={1} color="#ffffff" />
        <pointLight
          position={[-10, -10, -10]}
          intensity={0.5}
          color={props.isPlaying ? "#10b981" : "#3b82f6"}
        />

        <GlowingSphere {...props} />

        <Environment preset="city" />
        {/* Removed Stars component */}
      </Canvas>
    </div>
  );
}
